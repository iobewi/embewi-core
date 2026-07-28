// Package controller implémente les reconcile loops des CRDs Embewi.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/embewi/core/api/v1alpha1"
	"github.com/embewi/core/internal/agent"
	"github.com/embewi/core/internal/metrics"
)

const (
	// HeartbeatTimeout : si aucun heartbeat reçu depuis cette durée → ready=false.
	// Contrat §8a : Ready=True ← heartbeat reçu < 2 × période agent (5 s) = 10 s.
	HeartbeatTimeout = 10 * time.Second

	labelManagedBy = "embewi.io/managed-by"
	labelNodeID    = "embewi.io/node-id"

	// nodeMetricsFinalizer garantit metrics.RemoveNode() avant suppression effective
	// du McuNode — spec.NodeID (clé des gauges Prometheus) n'est plus lisible une fois
	// l'objet réellement supprimé, d'où le besoin d'intercepter via finalizer plutôt
	// que sur le NotFound du prochain reconcile.
	nodeMetricsFinalizer = "embewi.io/metrics-cleanup"
)

// McuNodeReconciler réconcilie les McuNode.
// Responsabilités :
//   - Créer/mettre à jour le Service selectorless + EndpointSlice associé (§8 contrat)
//   - Marquer ready=false si heartbeat trop ancien
//   - Mettre à jour les conditions Provisioned + Ready (§8a contrat)
type McuNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder // émet TokenRotation{Applied,Failed} (§4 contrat) ; nil = pas d'Events
}

func (r *McuNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var node v1alpha1.McuNode
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !node.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&node, nodeMetricsFinalizer) {
			metrics.RemoveNode(node.Spec.NodeID)
			controllerutil.RemoveFinalizer(&node, nodeMetricsFinalizer)
			if err := r.Update(ctx, &node); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&node, nodeMetricsFinalizer) {
		if err := r.Update(ctx, &node); err != nil {
			return ctrl.Result{}, err
		}
	}

	heartbeatExpired := node.Status.LastHeartbeat == nil ||
		time.Since(node.Status.LastHeartbeat.Time) > HeartbeatTimeout

	wantReady := !heartbeatExpired && node.Status.State == "running" && node.Status.OtaValidated
	wantState := node.Status.State
	if heartbeatExpired && node.Status.State != "offline" && node.Status.LastHeartbeat != nil {
		wantState = "offline"
	}

	// Capturer les valeurs avant mutation pour le guard de log (comparaison correcte).
	prevReady := node.Status.Ready
	prevState := node.Status.State

	// Verrou optimiste : le heartbeat POST (internal/heartbeat/server.go) patch le
	// même McuNode.Status de façon concurrente. Sans resourceVersion, un reconcile
	// basé sur une lecture pas encore rafraîchie peut écraser un heartbeat tout
	// juste arrivé (state/ready remis à "offline"/false). Avec le verrou, ce patch
	// échoue en conflit — controller-runtime réenqueue automatiquement l'erreur.
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Status.Ready = wantReady
	node.Status.State = wantState

	// Conditions §8a — pilotées par le timeout heartbeat.
	if node.Status.LastHeartbeat == nil {
		// Jamais enrôlé — device pas encore connecté.
		apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:    "Provisioned",
			Status:  metav1.ConditionFalse,
			Reason:  "ProvisioningPending",
			Message: "aucun heartbeat reçu, device non encore connecté",
		})
		apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionUnknown,
			Reason:  "NotProvisioned",
			Message: "device jamais enrôlé",
		})
	} else if heartbeatExpired {
		// Message sans time.Since() : valeur fixe pour que MergeFrom produise un diff vide
		// une fois la condition posée, évitant un Status().Patch() toutes les 30 s sur les
		// nodes offline.
		apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "HeartbeatTimeout",
			Message: fmt.Sprintf("aucun heartbeat depuis plus de %v", HeartbeatTimeout),
		})
	}

	if err := r.Status().Patch(ctx, &node, patch); err != nil {
		return ctrl.Result{}, err
	}
	if prevState != wantState || prevReady != wantReady {
		logger.Info("status →", "state", wantState, "ready", wantReady)
	}

	// Réconcilie Service + EndpointSlice uniquement si on a une IP.
	if node.Status.IP != "" {
		if err := r.reconcileService(ctx, &node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
		}
		if err := r.reconcileEndpointSlice(ctx, &node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile endpointslice: %w", err)
		}
		if err := r.reconcileTokenRotation(ctx, &node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile token rotation: %w", err)
		}
		if err := r.reconcileTLSCert(ctx, &node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile tls cert: %w", err)
		}
	}

	// Re-trigger avant expiry pour vérifier le timeout heartbeat.
	return ctrl.Result{RequeueAfter: HeartbeatTimeout}, nil
}

// reconcileService crée ou met à jour le Service selectorless pour ce McuNode.
func (r *McuNodeReconciler) reconcileService(ctx context.Context, node *v1alpha1.McuNode) error {
	svcName := "embewi-" + node.Name
	appPort := int32(8080)
	if node.Status.AppPort > 0 {
		appPort = int32(node.Status.AppPort)
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: node.Namespace,
			Labels: map[string]string{
				labelManagedBy: "embewi-controller",
				labelNodeID:    node.Spec.NodeID,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Name:     "app",
				Port:     appPort,
				Protocol: corev1.ProtocolTCP,
			}},
			// Selectorless : l'EndpointSlice pointe directement sur l'IP ESP.
		},
	}
	if err := controllerutil.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("SetControllerReference Service: %w", err)
	}

	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: node.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Ports = desired.Spec.Ports
	// Garantir la présence de l'OwnerReference sur les objets créés avant ce fix.
	if err := controllerutil.SetControllerReference(node, &existing, r.Scheme); err != nil {
		return fmt.Errorf("SetControllerReference Service existant: %w", err)
	}
	return r.Patch(ctx, &existing, patch)
}

// reconcileEndpointSlice met à jour l'EndpointSlice du Service avec l'IP et ready.
func (r *McuNodeReconciler) reconcileEndpointSlice(ctx context.Context, node *v1alpha1.McuNode) error {
	sliceName := "embewi-" + node.Name
	svcName := "embewi-" + node.Name
	ready := node.Status.Ready
	appPort := int32(8080)
	if node.Status.AppPort > 0 {
		appPort = int32(node.Status.AppPort)
	}
	proto := corev1.ProtocolTCP

	desired := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sliceName,
			Namespace: node.Namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name": svcName,
				labelManagedBy:               "embewi-controller",
				labelNodeID:                  node.Spec.NodeID,
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{node.Status.IP},
			Conditions: discoveryv1.EndpointConditions{
				Ready: &ready,
			},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     strPtr("app"),
			Port:     &appPort,
			Protocol: &proto,
		}},
	}
	if err := controllerutil.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("SetControllerReference EndpointSlice: %w", err)
	}

	var existing discoveryv1.EndpointSlice
	err := r.Get(ctx, types.NamespacedName{Name: sliceName, Namespace: node.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Endpoints = desired.Endpoints
	existing.Ports = desired.Ports
	// Garantir la présence de l'OwnerReference sur les objets créés avant ce fix.
	if err := controllerutil.SetControllerReference(node, &existing, r.Scheme); err != nil {
		return fmt.Errorf("SetControllerReference EndpointSlice existant: %w", err)
	}
	return r.Patch(ctx, &existing, patch)
}

// reconcileTokenRotation rejoue POST /token tant qu'une rotation reste non confirmée
// (§4 contrat). Déclenchement : l'opérateur (kubectl/GitOps) écrit dans le Secret
// référencé par node.Spec.TokenRef, EN MÊME TEMPS, data["token"]=newToken et
// data["previousToken"]=oldToken (rétention, une seule écriture atomique — cf.
// contrat §4 "protocole de rotation sans coupure"). Tant que previousToken est
// présent, le device n'a pas confirmé newToken : soit il ne l'a jamais reçu (POST
// /token pas encore tenté ou en échec), soit sa réponse au précédent essai s'est
// perdue avant que le Core ne puisse le constater — dans les deux cas, rejouer
// POST /token avec previousToken est sans risque (idempotent côté device : NVS
// commitée avant réponse, §4). La confirmation (et l'effacement de previousToken)
// se fait côté heartbeat, cf. clearPreviousToken.
func (r *McuNodeReconciler) reconcileTokenRotation(ctx context.Context, node *v1alpha1.McuNode) error {
	if node.Spec.TokenRef.Name == "" {
		return nil // Secret centralisé (legacy) : pas de support de rotation.
	}
	secretNS := node.Spec.TokenRef.Namespace
	if secretNS == "" {
		secretNS = node.Namespace
	}
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.TokenRef.Name, Namespace: secretNS}, &secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	previous := strings.TrimSpace(string(secret.Data["previousToken"]))
	if previous == "" {
		return nil // pas de rotation en cours
	}
	newToken := strings.TrimSpace(string(secret.Data["token"]))
	if newToken == "" {
		return nil // secret incohérent (previousToken sans token) : rien à jouer
	}

	logger := log.FromContext(ctx).WithValues("node", node.Spec.NodeID)
	if err := agent.New(node.Status.IP, previous).RotateToken(newToken); err != nil {
		// Pas fatal : le device peut être temporairement injoignable, ou avoir déjà
		// confirmé newToken entre-temps (previousToken pas encore effacé) — dans ce
		// dernier cas ce POST échoue en 401 côté device, sans conséquence : le
		// prochain heartbeat authentifié en newToken effacera previousToken.
		logger.Info("rotation token : POST /token a échoué, nouvelle tentative au prochain reconcile", "error", err.Error())
		if r.Recorder != nil {
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "TokenRotationFailed",
				"POST /token a échoué : %v (nouvelle tentative au prochain reconcile)", err)
		}
		return nil
	}
	logger.Info("rotation token : POST /token accepté par le device, confirmation attendue par heartbeat")
	if r.Recorder != nil {
		r.Recorder.Event(node, corev1.EventTypeNormal, "TokenRotationApplied",
			"POST /token accepté par le device ; confirmation attendue par heartbeat (newToken)")
	}
	return nil
}

// reconcileTLSCert pousse un nouveau certificat TLS si le Secret référencé par
// Spec.TLSSecretRef diverge du dernier digest appliqué avec succès (§4 contrat,
// POST /tls/cert). Contrairement à la config (§4a, generation/active_generation),
// l'agent n'expose aucun digest de cert via GET /info — le suivi de « déjà
// appliqué » se fait donc entièrement côté Core, via Status.TLSCertDigest.
// Un reboot est requis après le push (effectif au prochain embewi_http_start()) ;
// le digest n'est mis à jour qu'après un reboot confirmé accepté, pour ne pas
// perdre la trace d'un cert poussé mais pas encore actif en cas d'échec du reboot.
func (r *McuNodeReconciler) reconcileTLSCert(ctx context.Context, node *v1alpha1.McuNode) error {
	if node.Spec.TLSSecretRef.Name == "" {
		return nil // pas de cert géré par le Core : fallback auto-signé de build
	}
	if node.Spec.TokenRef.Name == "" {
		return nil // pas de support pour le Secret centralisé legacy (cf. reconcileTokenRotation)
	}

	tlsNS := node.Spec.TLSSecretRef.Namespace
	if tlsNS == "" {
		tlsNS = node.Namespace
	}
	var tlsSecret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.TLSSecretRef.Name, Namespace: tlsNS}, &tlsSecret); err != nil {
		return client.IgnoreNotFound(err)
	}
	certPEM := tlsSecret.Data["tls.crt"]
	keyPEM := tlsSecret.Data["tls.key"]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return fmt.Errorf("secret TLS %q/%q: tls.crt/tls.key manquants", tlsNS, node.Spec.TLSSecretRef.Name)
	}
	sum := sha256.Sum256(append(append([]byte{}, certPEM...), keyPEM...))
	digest := hex.EncodeToString(sum[:])
	if digest == node.Status.TLSCertDigest {
		return nil // déjà appliqué
	}

	tokenNS := node.Spec.TokenRef.Namespace
	if tokenNS == "" {
		tokenNS = node.Namespace
	}
	var tokenSecret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: node.Spec.TokenRef.Name, Namespace: tokenNS}, &tokenSecret); err != nil {
		return client.IgnoreNotFound(err)
	}
	token := strings.TrimSpace(string(tokenSecret.Data["token"]))
	if token == "" {
		return nil
	}

	logger := log.FromContext(ctx).WithValues("node", node.Spec.NodeID)
	cli := agent.New(node.Status.IP, token)
	if err := cli.PostTLSCert(string(certPEM), string(keyPEM)); err != nil {
		logger.Info("POST /tls/cert a échoué, nouvelle tentative au prochain reconcile", "error", err.Error())
		if r.Recorder != nil {
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "TLSCertRotationFailed",
				"POST /tls/cert a échoué : %v (nouvelle tentative au prochain reconcile)", err)
		}
		return nil
	}
	if err := cli.PostReboot(); err != nil {
		// Cert accepté côté device (NVS) mais reboot pas confirmé : ne pas marquer
		// TLSCertDigest comme appliqué, on retentera POST /tls/cert (idempotent) au
		// prochain reconcile.
		logger.Info("cert accepté mais POST /reboot a échoué, nouvelle tentative au prochain reconcile", "error", err.Error())
		if r.Recorder != nil {
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "TLSCertRotationFailed",
				"cert accepté mais POST /reboot a échoué : %v (nouvelle tentative au prochain reconcile)", err)
		}
		return nil
	}

	patch := client.MergeFrom(node.DeepCopy())
	node.Status.TLSCertDigest = digest
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return fmt.Errorf("patch TLSCertDigest: %w", err)
	}
	logger.Info("certificat TLS mis à jour, device rebooté")
	if r.Recorder != nil {
		r.Recorder.Event(node, corev1.EventTypeNormal, "TLSCertRotated", "certificat TLS mis à jour, device rebooté")
	}
	return nil
}

func (r *McuNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.McuNode{}).
		Complete(r)
}

func strPtr(s string) *string { return &s }
