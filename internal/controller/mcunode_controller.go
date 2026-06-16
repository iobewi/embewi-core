// Package controller implémente les reconcile loops des CRDs Embewi.
package controller

import (
	"context"
	"fmt"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/embewi/core/api/v1alpha1"
)

const (
	// HeartbeatTimeout : si aucun heartbeat reçu depuis cette durée → ready=false.
	HeartbeatTimeout = 30 * time.Second

	labelManagedBy = "embewi.io/managed-by"
	labelNodeID    = "embewi.io/node-id"
)

// McuNodeReconciler réconcilie les McuNode.
// Responsabilités :
//   - Créer/mettre à jour le Service selectorless + EndpointSlice associé (§8 contrat)
//   - Marquer ready=false si heartbeat trop ancien
type McuNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	// Calcule ready selon §8 contrat :
	//   true  ssi state==running && ota_validated==true && heartbeat récent
	//   false pour pending_verify, degraded, rollback, failed, timeout
	wantReady := false
	if node.Status.State == "running" && node.Status.OtaValidated &&
		node.Status.LastHeartbeat != nil &&
		time.Since(node.Status.LastHeartbeat.Time) <= HeartbeatTimeout {
		wantReady = true
	}

	if wantReady != node.Status.Ready {
		reason := "heartbeat timeout"
		if node.Status.State != "running" || !node.Status.OtaValidated {
			reason = "state=" + node.Status.State
		}
		patch := client.MergeFrom(node.DeepCopy())
		node.Status.Ready = wantReady
		if err := r.Status().Patch(ctx, &node, patch); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("ready →", "ready", wantReady, "reason", reason)
	}

	// Réconcilie Service + EndpointSlice uniquement si on a une IP.
	if node.Status.IP != "" {
		if err := r.reconcileService(ctx, &node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
		}
		if err := r.reconcileEndpointSlice(ctx, &node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile endpointslice: %w", err)
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
	return r.Patch(ctx, &existing, patch)
}

// reconcileEndpointSlice met à jour l'EndpointSlice du Service avec l'IP et ready.
func (r *McuNodeReconciler) reconcileEndpointSlice(ctx context.Context, node *v1alpha1.McuNode) error {
	sliceName := "embewi-" + node.Name
	svcName   := "embewi-" + node.Name
	ready     := node.Status.Ready
	appPort   := int32(8080)
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
				labelManagedBy:              "embewi-controller",
				labelNodeID:                 node.Spec.NodeID,
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
	existing.Ports      = desired.Ports
	return r.Patch(ctx, &existing, patch)
}

func (r *McuNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.McuNode{}).
		Complete(r)
}

func strPtr(s string) *string { return &s }
