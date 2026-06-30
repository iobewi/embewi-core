package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/embewi/core/api/v1alpha1"
	"github.com/embewi/core/internal/agent"
	"github.com/embewi/core/internal/oci"
)

// ConfirmTimeout : délai max pour recevoir state=running après activate (§3 contrat).
const ConfirmTimeout = 2 * time.Minute

// annotationConfirmingSince : timestamp RFC3339 d'entrée en phase Confirming.
// Permet le timeout négatif horodaté sans champ CRD supplémentaire.
const annotationConfirmingSince = "embewi.io/confirming-since"

// McuDeploymentReconciler orchestre le déploiement OTA sur l'agent ESP.
//
// Machine d'état pilotée par McuDeployment.Status.Phase :
//
//	""          → Binding   (résoudre le McuNode cible)
//	Binding     → Pulling   (pull manifeste OCI → Digest + Size)
//	Pulling     → Preparing (POST /ota/prepare, idempotent via staged §6)
//	Preparing   → Writing   (PUT /ota/write, stream blob OCI → ESP)
//	Writing     → Activating(POST /ota/activate + reboot)
//	Activating  → Confirming(attente heartbeat running + ota_validated)
//	Confirming  → Deployed  (confirmation reçue via heartbeat)
//	*           → Failed    (erreur terminale ou timeout négatif)
type McuDeploymentReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	OCI         *oci.Client // client OCI pour pull firmware
	TokenSecret string      // nom du Secret K8s contenant les tokens Bearer (défaut : "embewi-tokens")
}

func (r *McuDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var dep v1alpha1.McuDeployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Déploiement terminal — plus de réconciliation.
	if dep.Status.Phase == v1alpha1.PhaseDeployed || dep.Status.Phase == v1alpha1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	switch dep.Status.Phase {
	case "":
		return r.phaseBinding(ctx, &dep)
	case v1alpha1.PhaseBinding:
		return r.phaseBinding(ctx, &dep)
	case v1alpha1.PhasePulling:
		return r.phasePulling(ctx, &dep)
	case v1alpha1.PhasePreparing:
		return r.phasePreparing(ctx, &dep)
	case v1alpha1.PhaseWriting:
		return r.phaseWriting(ctx, &dep)
	case v1alpha1.PhaseActivating:
		return r.phaseActivating(ctx, &dep)
	case v1alpha1.PhaseConfirming:
		return r.phaseConfirming(ctx, &dep)
	default:
		logger.Info("phase inconnue", "phase", dep.Status.Phase)
		return ctrl.Result{}, nil
	}
}

// phaseBinding résout le McuNode cible selon nodeName ou nodeSelector (§7).
func (r *McuDeploymentReconciler) phaseBinding(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var nodeList v1alpha1.McuNodeList
	if err := r.List(ctx, &nodeList, client.InNamespace(dep.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	var candidates []v1alpha1.McuNode
	for _, n := range nodeList.Items {
		if dep.Spec.NodeName != "" {
			if n.Name == dep.Spec.NodeName {
				candidates = append(candidates, n)
			}
		} else {
			match := true
			for k, v := range dep.Spec.NodeSelector {
				if n.Labels[k] != v {
					match = false
					break
				}
			}
			if match {
				candidates = append(candidates, n)
			}
		}
	}

	switch len(candidates) {
	case 0:
		return r.fail(ctx, dep, "NoDeviceMatched", "aucun McuNode ne correspond")
	case 1:
		if err := r.checkNotBusy(ctx, dep, candidates[0].Name); err != nil {
			return r.fail(ctx, dep, "DeviceBusy", err.Error())
		}
		logger.Info("McuNode résolu", "node", candidates[0].Name)
		return r.setPhase(ctx, dep, v1alpha1.PhasePulling, candidates[0].Name, "")
	default:
		return r.fail(ctx, dep, "AmbiguousBinding",
			fmt.Sprintf("%d nodes correspondent, pin explicite requis", len(candidates)))
	}
}

// checkNotBusy vérifie qu'aucun autre McuDeployment n'est déjà bindé sur ce node.
func (r *McuDeploymentReconciler) checkNotBusy(ctx context.Context, dep *v1alpha1.McuDeployment, nodeName string) error {
	var list v1alpha1.McuDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(dep.Namespace)); err != nil {
		return err
	}
	for _, d := range list.Items {
		if d.Name == dep.Name {
			continue
		}
		if d.Status.BoundNode == nodeName &&
			d.Status.Phase != v1alpha1.PhaseDeployed &&
			d.Status.Phase != v1alpha1.PhaseFailed {
			return fmt.Errorf("node %q déjà utilisé par McuDeployment %q", nodeName, d.Name)
		}
	}
	return nil
}

// phasePulling : résout le manifeste OCI et stocke Digest + Size dans Status.
// DeploymentID est fixé à string(dep.UID) — stable et unique par objet.
func (r *McuDeploymentReconciler) phasePulling(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	meta, err := r.OCI.ResolveFirmware(ctx, dep.Spec.Firmware.Image)
	if err != nil {
		logger.Error(err, "résolution firmware OCI échouée", "image", dep.Spec.Firmware.Image)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	patch := client.MergeFrom(dep.DeepCopy())
	dep.Status.Phase = v1alpha1.PhasePreparing
	if dep.Status.DeploymentID == "" {
		dep.Status.DeploymentID = string(dep.UID)
	}
	dep.Status.Digest = meta.Digest
	dep.Status.Size = meta.Size
	setDeploymentConditions(dep, v1alpha1.PhasePreparing, "")
	if err := r.Status().Patch(ctx, dep, patch); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("firmware OCI résolu",
		"digest", meta.Digest,
		"size", meta.Size,
		"chip", meta.Chip,
		"idf", meta.IDFVersion)
	return ctrl.Result{Requeue: true}, nil
}

// phasePreparing : POST /ota/prepare avec idempotence (§6).
func (r *McuDeploymentReconciler) phasePreparing(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	node, cli, err := r.nodeClient(ctx, dep)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Idempotence §6 : lire staged pour décider où reprendre après un crash Core.
	info, err := cli.GetInfo()
	if err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	switch {
	case info.Staged.State == "activating" && info.Staged.DeploymentID == dep.Status.DeploymentID:
		log.FromContext(ctx).Info("staged=activating (idempotence) → skip write+activate, attente heartbeat")
		return r.setPhase(ctx, dep, v1alpha1.PhaseConfirming, node.Name, "")
	case info.Staged.State == "written" && info.Staged.Digest == dep.Status.Digest &&
		info.Staged.DeploymentID == dep.Status.DeploymentID:
		log.FromContext(ctx).Info("staged=written (idempotence) → skip write")
		return r.setPhase(ctx, dep, v1alpha1.PhaseActivating, node.Name, "")
	}

	resp, err := cli.OTAPrepare(agent.PrepareRequest{
		DeploymentID:    dep.Status.DeploymentID,
		Digest:          dep.Status.Digest,
		Size:            dep.Status.Size,
		Chip:            node.Status.Chip,
		IDFVersion:      node.Status.IDFVersion,
		PartitionLayout: "embewi-ab-v1",
	})
	if err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	if !resp.Accepted {
		return r.fail(ctx, dep, resp.Reason, fmt.Sprintf("prepare refusé: %s", resp.Reason))
	}

	return r.setPhase(ctx, dep, v1alpha1.PhaseWriting, node.Name, "")
}

// phaseWriting : stream du blob OCI vers PUT /ota/write.
func (r *McuDeploymentReconciler) phaseWriting(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	_, cli, err := r.nodeClient(ctx, dep)
	if err != nil {
		return ctrl.Result{}, err
	}

	stream, err := r.OCI.StreamBlob(ctx, dep.Spec.Firmware.Image, dep.Status.Digest)
	if err != nil {
		log.FromContext(ctx).Error(err, "stream blob OCI échoué", "digest", dep.Status.Digest)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, fmt.Errorf("stream blob: %w", err)
	}
	defer stream.Close()

	if err := cli.OTAWrite(dep.Status.DeploymentID, dep.Status.Digest, dep.Status.Size, stream); err != nil {
		log.FromContext(ctx).Error(err, "OTAWrite échoué")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, fmt.Errorf("OTAWrite: %w", err)
	}

	log.FromContext(ctx).Info("firmware écrit avec succès", "size", dep.Status.Size)
	return r.setPhase(ctx, dep, v1alpha1.PhaseActivating, dep.Status.BoundNode, "")
}

// phaseActivating : POST /ota/activate → reboot device.
func (r *McuDeploymentReconciler) phaseActivating(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	_, cli, err := r.nodeClient(ctx, dep)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := cli.OTAActivate(dep.Status.DeploymentID); err != nil {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, err
	}
	return r.setPhase(ctx, dep, v1alpha1.PhaseConfirming, dep.Status.BoundNode, "")
}

// phaseConfirming : attente du heartbeat running + ota_validated (timeout négatif §3).
// Le timestamp d'entrée est stocké dans l'annotation annotationConfirmingSince.
func (r *McuDeploymentReconciler) phaseConfirming(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	node, _, err := r.nodeClient(ctx, dep)
	if err != nil {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}

	// Confirmation reçue.
	if node.Status.State == "running" &&
		node.Status.OtaValidated &&
		node.Status.DeploymentID == dep.Status.DeploymentID {
		return r.setPhase(ctx, dep, v1alpha1.PhaseDeployed, node.Name, "déploiement confirmé par heartbeat")
	}

	// Rollback détecté — le device a rebooté sur l'ancienne image.
	if node.Status.State == "failed" || node.Status.State == "rollback" {
		return r.fail(ctx, dep, "DeviceRollback",
			fmt.Sprintf("device en état %q après activation", node.Status.State))
	}

	// Timeout négatif horodaté (§3) : si la confirmation n'arrive pas dans ConfirmTimeout → Failed.
	if sinceStr, ok := dep.Annotations[annotationConfirmingSince]; ok {
		since, err := time.Parse(time.RFC3339, sinceStr)
		if err == nil && time.Since(since) > ConfirmTimeout {
			logger.Error(nil, "timeout négatif dépassé", "since", sinceStr, "timeout", ConfirmTimeout)
			return r.fail(ctx, dep, "ConfirmTimeout",
				fmt.Sprintf("aucune confirmation dans les %v après activate", ConfirmTimeout))
		}
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// nodeToken charge le token Bearer depuis le Secret K8s r.TokenSecret.
// Clé du Secret = node.Spec.NodeID (ex: "esp32-motor-left").
func (r *McuDeploymentReconciler) nodeToken(ctx context.Context, ns, nodeID string) (string, error) {
	secretName := r.TokenSecret
	if secretName == "" {
		secretName = "embewi-tokens"
	}
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: ns}, &secret); err != nil {
		return "", fmt.Errorf("secret %q/%q: %w", ns, secretName, err)
	}
	token, ok := secret.Data[nodeID]
	if !ok {
		return "", fmt.Errorf("clé %q absente du secret %q (namespace %s)", nodeID, secretName, ns)
	}
	return strings.TrimSpace(string(token)), nil
}

// nodeClient retourne le McuNode résolu et un agent.Client configuré avec le token du Secret.
func (r *McuDeploymentReconciler) nodeClient(ctx context.Context, dep *v1alpha1.McuDeployment) (*v1alpha1.McuNode, *agent.Client, error) {
	var node v1alpha1.McuNode
	if err := r.Get(ctx, client.ObjectKey{Name: dep.Status.BoundNode, Namespace: dep.Namespace}, &node); err != nil {
		return nil, nil, fmt.Errorf("McuNode %q: %w", dep.Status.BoundNode, err)
	}
	if node.Status.IP == "" {
		return nil, nil, fmt.Errorf("McuNode %q: IP inconnue (aucun heartbeat reçu)", node.Name)
	}
	token, err := r.nodeToken(ctx, dep.Namespace, node.Spec.NodeID)
	if err != nil {
		return nil, nil, err
	}
	return &node, agent.New(node.Status.IP, token), nil
}

func (r *McuDeploymentReconciler) setPhase(ctx context.Context, dep *v1alpha1.McuDeployment,
	phase v1alpha1.McuDeploymentPhase, boundNode, msg string) (ctrl.Result, error) {

	// Horodatage d'entrée en Confirming pour le timeout négatif (§3).
	if phase == v1alpha1.PhaseConfirming {
		if dep.Annotations == nil {
			dep.Annotations = map[string]string{}
		}
		if _, already := dep.Annotations[annotationConfirmingSince]; !already {
			metaPatch := client.MergeFrom(dep.DeepCopy())
			dep.Annotations[annotationConfirmingSince] = time.Now().UTC().Format(time.RFC3339)
			if err := r.Patch(ctx, dep, metaPatch); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	patch := client.MergeFrom(dep.DeepCopy())
	dep.Status.Phase = phase
	dep.Status.BoundNode = boundNode
	dep.Status.Message = msg
	setDeploymentConditions(dep, phase, msg)
	if err := r.Status().Patch(ctx, dep, patch); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("phase →", "phase", phase, "node", boundNode)
	return ctrl.Result{Requeue: true}, nil
}

func (r *McuDeploymentReconciler) fail(ctx context.Context, dep *v1alpha1.McuDeployment, reason, msg string) (ctrl.Result, error) {
	patch := client.MergeFrom(dep.DeepCopy())
	dep.Status.Phase = v1alpha1.PhaseFailed
	dep.Status.Message = fmt.Sprintf("[%s] %s", reason, msg)
	apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
		Type:    "Progressing",
		Status:  metav1.ConditionFalse,
		Reason:  "OTAFailed",
		Message: msg,
	})
	apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
		Type:    "Available",
		Status:  metav1.ConditionFalse,
		Reason:  "DeviceDegraded",
		Message: fmt.Sprintf("[%s] %s", reason, msg),
	})
	if err := r.Status().Patch(ctx, dep, patch); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Error(nil, "déploiement échoué", "reason", reason, "msg", msg)
	return ctrl.Result{}, nil
}

// setDeploymentConditions met à jour Progressing + Available selon la phase (§8a contrat).
func setDeploymentConditions(dep *v1alpha1.McuDeployment, phase v1alpha1.McuDeploymentPhase, msg string) {
	switch phase {
	case v1alpha1.PhaseDeployed:
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionFalse,
			Reason:  "DeploymentComplete",
			Message: "OTA terminé, firmware stable",
		})
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionTrue,
			Reason:  "WorkloadReady",
			Message: "EndpointSlice.ready=true, workload joignable",
		})
	case v1alpha1.PhaseConfirming:
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionTrue,
			Reason:  "OTAInProgress",
			Message: "attente confirmation device (pending_verify)",
		})
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionFalse,
			Reason:  "PendingVerification",
			Message: "image non encore validée par le device",
		})
	default:
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionTrue,
			Reason:  "OTAInProgress",
			Message: fmt.Sprintf("phase: %s", phase),
		})
	}
}

// nodeToDeployments mappe un McuNode vers les McuDeployment en phase Confirming qui l'attendent.
// Appelé par le Watches sur McuNode — permet de re-trigger immédiatement à chaque heartbeat
// plutôt que d'attendre le RequeueAfter de 10 s.
func (r *McuDeploymentReconciler) nodeToDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*v1alpha1.McuNode)
	if !ok {
		return nil
	}
	var list v1alpha1.McuDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(node.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, dep := range list.Items {
		if dep.Status.Phase == v1alpha1.PhaseConfirming && dep.Status.BoundNode == node.Name {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      dep.Name,
					Namespace: dep.Namespace,
				},
			})
		}
	}
	return reqs
}

func (r *McuDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.McuDeployment{}).
		Watches(&v1alpha1.McuNode{}, handler.EnqueueRequestsFromMapFunc(r.nodeToDeployments)).
		Complete(r)
}
