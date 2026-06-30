package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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

// errConfigPermanent indique une erreur de configuration non-transitoire (clé trop longue,
// McuConfigMap introuvable). L'opérateur doit corriger le McuConfigMap avant tout progrès.
// Utilisé pour distinguer les erreurs permanentes des erreurs réseau transitoires.
var errConfigPermanent = stderrors.New("erreur de configuration permanente")

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
	OCI         *oci.Client          // client OCI pour pull firmware
	TokenSecret string               // nom du Secret K8s contenant les tokens Bearer (défaut : "embewi-tokens")
	Recorder    record.EventRecorder // émet des Events K8s (§4b contrat)
}

func (r *McuDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var dep v1alpha1.McuDeployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Déploiement Failed : terminal.
	if dep.Status.Phase == v1alpha1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	// Déploiement Deployed : surveille Available/HeartbeatTimeout et traite les mises à jour config.
	if dep.Status.Phase == v1alpha1.PhaseDeployed {
		return r.reconcileDeployed(ctx, &dep)
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
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

// pushConfigIfNeeded compare le McuConfigMap avec le NVS du device et pousse si divergent.
// Sémantique merge-on-key : seules les clés citées dans Data sont vérifiées/écrites.
// Les clés réservées (préfixe `_`) sont ignorées — l'agent les filtre silencieusement en NVS.
// Retourne true si une mise à jour a été poussée.
// Retourne errConfigPermanent pour les erreurs non-transitoires (clé trop longue, CM absent).
func (r *McuDeploymentReconciler) pushConfigIfNeeded(ctx context.Context, cli *agent.Client, dep *v1alpha1.McuDeployment) (bool, error) {
	var cm v1alpha1.McuConfigMap
	if err := r.Get(ctx, client.ObjectKey{Name: dep.Spec.ConfigMapRef, Namespace: dep.Namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("%w: McuConfigMap %q introuvable", errConfigPermanent, dep.Spec.ConfigMapRef)
		}
		return false, fmt.Errorf("McuConfigMap %q: %w", dep.Spec.ConfigMapRef, err)
	}

	// Validation limites NVS agent (§4a contrat) — erreurs permanentes.
	for k, v := range cm.Data {
		if len(k) > 15 {
			return false, fmt.Errorf("%w: clé %q dépasse 15 caractères (limite NVS §4a)", errConfigPermanent, k)
		}
		if len(v) > 63 {
			return false, fmt.Errorf("%w: valeur de %q dépasse 63 caractères (limite NVS §4a)", errConfigPermanent, k)
		}
	}

	current, err := cli.GetConfig()
	if err != nil {
		return false, fmt.Errorf("GET /config: %w", err)
	}

	// Construire le payload filtré (clés réservées `_` ignorées par l'agent NVS).
	// Les comparer avec le NVS courant pour détecter la divergence.
	filtered := make(map[string]string, len(cm.Data))
	needsPush := false
	for k, v := range cm.Data {
		if strings.HasPrefix(k, "_") {
			continue // réservé agent — jamais stocké en NVS, jamais comparé
		}
		filtered[k] = v
		if current.NVS[k] != v {
			needsPush = true
		}
	}
	if !needsPush {
		return false, nil
	}

	if err := cli.PostConfig(filtered); err != nil {
		return false, fmt.Errorf("POST /config: %w", err)
	}
	log.FromContext(ctx).Info("config poussée vers le device", "configMapRef", dep.Spec.ConfigMapRef)
	return true, nil
}

// reconcileConfigOnly traite les mises à jour config sur un McuDeployment en phase Deployed.
// Appelé quand le McuConfigMap référencé change (§7a contrat).
// Pousse la config et reboot le device si le NVS diverge.
func (r *McuDeploymentReconciler) reconcileConfigOnly(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	_, cli, err := r.nodeClient(ctx, dep)
	if err != nil {
		// Device peut être temporairement offline — on réessaie.
		logger.Error(err, "nodeClient unavailable, retry dans 30s")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	pushed, err := r.pushConfigIfNeeded(ctx, cli, dep)
	if err != nil {
		if stderrors.Is(err, errConfigPermanent) {
			return r.fail(ctx, dep, "ConfigInvalid", err.Error())
		}
		logger.Error(err, "pushConfigIfNeeded échoué (transitoire)")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if pushed {
		if err := cli.PostReboot(); err != nil {
			logger.Error(err, "POST /reboot échoué après config update")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		logger.Info("config mise à jour + reboot", "configMapRef", dep.Spec.ConfigMapRef)
	}

	return ctrl.Result{}, nil
}

// configMapToDeployments mappe un McuConfigMap vers les McuDeployment qui le référencent.
// Déclenche la réconciliation config-only sur les Deployed (§7a contrat).
func (r *McuDeploymentReconciler) configMapToDeployments(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*v1alpha1.McuConfigMap)
	if !ok {
		return nil
	}
	var list v1alpha1.McuDeploymentList
	if err := r.List(ctx, &list, client.InNamespace(cm.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, dep := range list.Items {
		if dep.Spec.ConfigMapRef == cm.Name {
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

// phasePreparing : POST /ota/prepare avec idempotence (§6).
func (r *McuDeploymentReconciler) phasePreparing(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	node, cli, err := r.nodeClient(ctx, dep)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Pousser la config AVANT l'OTA (§6 contrat — POST /config d'abord, OTA ensuite).
	if dep.Spec.ConfigMapRef != "" {
		if _, err := r.pushConfigIfNeeded(ctx, cli, dep); err != nil {
			if stderrors.Is(err, errConfigPermanent) {
				return r.fail(ctx, dep, "ConfigInvalid", err.Error())
			}
			log.FromContext(ctx).Error(err, "push config avant OTA échoué (transitoire)")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	// Idempotence §6 : lire staged pour décider où reprendre après un crash Core.
	// Persiste aussi chip/idf/appPort dans McuNode.Status (§8 contrat).
	info, err := cli.GetInfo()
	if err != nil {
		log.FromContext(ctx).Error(err, "GET /info échoué (transitoire)")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	r.persistNodeInfo(ctx, node, info)
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
		Artifact:        dep.Spec.Firmware.Image,
		Digest:          dep.Status.Digest,
		Size:            dep.Status.Size,
		Chip:            node.Status.Chip,
		IDFVersion:      node.Status.IDFVersion,
		PartitionLayout: "embewi-ab-v1",
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "POST /ota/prepare échoué (transitoire)")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if !resp.Accepted {
		// Mapper les codes §4b → Events K8s stables.
		eventReason := map[string]string{
			"chip_mismatch":    "OTARejectedChip",
			"layout_mismatch":  "OTARejectedLayout",
			"idf_incompatible": "OTARejectedIdf",
			"size_too_large":   "OTARejectedSize",
			"busy":             "OTABusy",
		}[resp.Reason]
		if eventReason == "" {
			eventReason = "OTARejected"
		}
		r.Recorder.Event(dep, corev1.EventTypeWarning, eventReason,
			fmt.Sprintf("prepare refusé par le device: %s", resp.Reason))
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
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	defer stream.Close()

	if err := cli.OTAWrite(dep.Status.DeploymentID, dep.Status.Digest, dep.Status.Size, stream); err != nil {
		log.FromContext(ctx).Error(err, "OTAWrite échoué")
		// Mapper les codes §4b → Events K8s stables.
		var writeErr *agent.OTAWriteError
		if stderrors.As(err, &writeErr) {
			switch writeErr.Status {
			case "digest_mismatch":
				r.Recorder.Event(dep, corev1.EventTypeWarning, "OTADigestMismatch", writeErr.Error())
			case "write_failed":
				r.Recorder.Event(dep, corev1.EventTypeWarning, "OTAWriteFailed", writeErr.Error())
			case "ota_begin_failed":
				r.Recorder.Event(dep, corev1.EventTypeWarning, "OTABeginFailed", writeErr.Error())
			// range_mismatch (416) : resync attendu — pas d'event, on réessaie.
			}
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	log.FromContext(ctx).Info("firmware écrit avec succès", "size", dep.Status.Size)
	r.Recorder.Event(dep, corev1.EventTypeNormal, "OTAWritten",
		fmt.Sprintf("firmware écrit sur le device (%d bytes)", dep.Status.Size))
	return r.setPhase(ctx, dep, v1alpha1.PhaseActivating, dep.Status.BoundNode, "")
}

// phaseActivating : POST /ota/activate → reboot device.
func (r *McuDeploymentReconciler) phaseActivating(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	node, cli, err := r.nodeClient(ctx, dep)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Idempotence : si le patch setPhase→Confirming a échoué après un activate réussi,
	// le device peut déjà être en staged=activating ou en pending_verify après reboot.
	// Ne pas envoyer un second OTAActivate dans ces cas.
	info, err := cli.GetInfo()
	if err != nil {
		log.FromContext(ctx).Error(err, "GET /info avant activate échoué (transitoire)")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	r.persistNodeInfo(ctx, node, info)
	alreadyActivated :=
		(info.Staged.State == "activating" && info.Staged.DeploymentID == dep.Status.DeploymentID) ||
			info.State == "pending_verify"
	if !alreadyActivated {
		if err := cli.OTAActivate(dep.Status.DeploymentID); err != nil {
			log.FromContext(ctx).Error(err, "POST /ota/activate échoué (transitoire)")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}
	return r.setPhase(ctx, dep, v1alpha1.PhaseConfirming, dep.Status.BoundNode, "")
}

// phaseConfirming : attente du heartbeat running + ota_validated (timeout négatif §3).
// Le timestamp d'entrée est stocké dans l'annotation annotationConfirmingSince.
func (r *McuDeploymentReconciler) phaseConfirming(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	node, _, err := r.nodeClient(ctx, dep)
	if err != nil {
		log.FromContext(ctx).Error(err, "nodeClient indisponible en Confirming (transitoire)")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Confirmation reçue.
	if node.Status.State == "running" &&
		node.Status.OtaValidated &&
		node.Status.DeploymentID == dep.Status.DeploymentID {
		return r.setPhase(ctx, dep, v1alpha1.PhaseDeployed, node.Name, "déploiement confirmé par heartbeat")
	}

	// Rollback/degraded détectés — le device n'a pas validé le nouveau firmware.
	switch node.Status.State {
	case "failed", "rollback", "degraded":
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
	setReadyCondition(dep)
	if err := r.Status().Patch(ctx, dep, patch); err != nil {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).Error(nil, "déploiement échoué", "reason", reason, "msg", msg)
	return ctrl.Result{}, nil
}

// setReadyCondition dérive la condition Ready synthétique (§8a contrat).
// Ready=True ← Progressing=False ET Available=True.
// Compatible : kubectl wait mcudeployment/x --for=condition=Ready
func setReadyCondition(dep *v1alpha1.McuDeployment) {
	progressing := apimeta.FindStatusCondition(dep.Status.Conditions, "Progressing")
	available := apimeta.FindStatusCondition(dep.Status.Conditions, "Available")
	if progressing != nil && available != nil &&
		progressing.Status == metav1.ConditionFalse &&
		available.Status == metav1.ConditionTrue {
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionTrue,
			Reason:  "DeploymentReady",
			Message: "Progressing=False et Available=True",
		})
	} else {
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "DeploymentNotReady",
			Message: "déploiement en cours ou device non disponible",
		})
	}
}

// setDeploymentConditions met à jour Progressing + Available + Ready selon la phase (§8a contrat).
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
	setReadyCondition(dep)
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
		if dep.Status.BoundNode != node.Name {
			continue
		}
		// PhaseConfirming : attente heartbeat running + ota_validated.
		// PhaseDeployed   : surveillance Available/HeartbeatTimeout (§8a contrat).
		if dep.Status.Phase == v1alpha1.PhaseConfirming || dep.Status.Phase == v1alpha1.PhaseDeployed {
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

// persistNodeInfo persiste les capacités hardware depuis GET /info dans McuNode.Status.
// Chip, IDFVersion et AppPort sont inconnus jusqu'au premier contact — ne bloquer jamais sur cette erreur.
func (r *McuDeploymentReconciler) persistNodeInfo(ctx context.Context, node *v1alpha1.McuNode, info *agent.InfoResponse) {
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.Chip = info.Chip
	node.Status.IDFVersion = info.IDFVersion
	node.Status.FlashSize = info.FlashSize
	node.Status.RAMSize = info.RAMSize
	if info.AppPort != 0 {
		node.Status.AppPort = info.AppPort
	}
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		log.FromContext(ctx).Error(err, "patch McuNode hardware info échoué (non bloquant)")
	}
}

// reconcileDeployed gère un McuDeployment en phase Deployed :
//   - Available/HeartbeatTimeout si le node ne répond plus (§8a contrat)
//   - Push config si McuConfigMap diverge (§7a contrat)
func (r *McuDeploymentReconciler) reconcileDeployed(ctx context.Context, dep *v1alpha1.McuDeployment) (ctrl.Result, error) {
	var node v1alpha1.McuNode
	if err := r.Get(ctx, client.ObjectKey{Name: dep.Status.BoundNode, Namespace: dep.Namespace}, &node); err != nil {
		log.FromContext(ctx).Error(err, "McuNode introuvable en phase Deployed")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	heartbeatExpired := node.Status.LastHeartbeat == nil ||
		time.Since(node.Status.LastHeartbeat.Time) > HeartbeatTimeout

	patch := client.MergeFrom(dep.DeepCopy())
	if heartbeatExpired {
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionFalse,
			Reason:  "HeartbeatTimeout",
			Message: fmt.Sprintf("aucun heartbeat depuis plus de %v", HeartbeatTimeout),
		})
	} else {
		apimeta.SetStatusCondition(&dep.Status.Conditions, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionTrue,
			Reason:  "WorkloadReady",
			Message: "EndpointSlice.ready=true, workload joignable",
		})
	}
	setReadyCondition(dep)
	if err := r.Status().Patch(ctx, dep, patch); err != nil {
		return ctrl.Result{}, err
	}

	if dep.Spec.ConfigMapRef != "" {
		return r.reconcileConfigOnly(ctx, dep)
	}
	return ctrl.Result{}, nil
}

func (r *McuDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.McuDeployment{}).
		Watches(&v1alpha1.McuNode{}, handler.EnqueueRequestsFromMapFunc(r.nodeToDeployments)).
		Watches(&v1alpha1.McuConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.configMapToDeployments)).
		Complete(r)
}
