package controller_test

import (
	"context"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/embewi/core/api/v1alpha1"
	"github.com/embewi/core/internal/controller"
)

func nodeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func reconcileNode(t *testing.T, r *controller.McuNodeReconciler, name, ns string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("Reconcile McuNode error : %v", err)
	}
	return result
}

// TestReconcile_NeverReceived vérifie les conditions quand aucun heartbeat n'a été reçu.
func TestReconcile_NeverReceived(t *testing.T) {
	scheme := nodeScheme(t)
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "esp-new", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-new-id"},
		// Status vide : aucun heartbeat reçu.
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	r := &controller.McuNodeReconciler{Client: fc, Scheme: scheme}
	reconcileNode(t, r, node.Name, node.Namespace)

	var updated v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, &updated) //nolint:errcheck

	cProvision := apimeta.FindStatusCondition(updated.Status.Conditions, "Provisioned")
	if cProvision == nil {
		t.Fatal("condition Provisioned absente")
	}
	if cProvision.Status != metav1.ConditionFalse {
		t.Errorf("Provisioned : got %q, want False (jamais enrôlé)", cProvision.Status)
	}
	if cProvision.Reason != "ProvisioningPending" {
		t.Errorf("Provisioned reason : got %q, want ProvisioningPending", cProvision.Reason)
	}

	cReady := apimeta.FindStatusCondition(updated.Status.Conditions, "Ready")
	if cReady == nil {
		t.Fatal("condition Ready absente")
	}
	if cReady.Status != metav1.ConditionUnknown {
		t.Errorf("Ready : got %q, want Unknown (jamais enrôlé)", cReady.Status)
	}
	if cReady.Reason != "NotProvisioned" {
		t.Errorf("Ready reason : got %q, want NotProvisioned", cReady.Reason)
	}
}

// TestReconcile_HeartbeatExpired vérifie le timeout heartbeat → état offline + Ready=False.
func TestReconcile_HeartbeatExpired(t *testing.T) {
	scheme := nodeScheme(t)
	old := metav1.NewTime(time.Now().Add(-2 * time.Minute)) // bien au-delà du timeout de 30s
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "esp-timeout", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-timeout-id"},
		Status: v1alpha1.McuNodeStatus{
			IP:            "192.168.1.10",
			State:         "running",
			LastHeartbeat: &old,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	r := &controller.McuNodeReconciler{Client: fc, Scheme: scheme}
	reconcileNode(t, r, node.Name, node.Namespace)

	var updated v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, &updated) //nolint:errcheck

	if updated.Status.State != "offline" {
		t.Errorf("State après timeout : got %q, want offline", updated.Status.State)
	}
	if updated.Status.Ready {
		t.Error("Ready après timeout : got true, want false")
	}

	cReady := apimeta.FindStatusCondition(updated.Status.Conditions, "Ready")
	if cReady == nil {
		t.Fatal("condition Ready absente")
	}
	if cReady.Status != metav1.ConditionFalse {
		t.Errorf("Ready : got %q, want False", cReady.Status)
	}
	if cReady.Reason != "HeartbeatTimeout" {
		t.Errorf("Ready reason : got %q, want HeartbeatTimeout", cReady.Reason)
	}
}

// TestReconcile_WithIP_CreatesServiceAndEndpointSlice vérifie la création du Service
// et de l'EndpointSlice quand le node a une IP.
func TestReconcile_WithIP_CreatesServiceAndEndpointSlice(t *testing.T) {
	scheme := nodeScheme(t)
	recent := metav1.NewTime(time.Now())
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "esp-svc", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-svc-id"},
		Status: v1alpha1.McuNodeStatus{
			IP:            "10.0.0.5",
			State:         "running",
			OtaValidated:  true,
			Ready:         true,
			LastHeartbeat: &recent,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	r := &controller.McuNodeReconciler{Client: fc, Scheme: scheme}
	reconcileNode(t, r, node.Name, node.Namespace)

	// Vérifier que le Service a été créé.
	var svc corev1.Service
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "embewi-esp-svc", Namespace: "embewi"}, &svc); err != nil {
		t.Errorf("Service non créé : %v", err)
	}

	// Vérifier que l'EndpointSlice a été créé avec la bonne IP.
	var eps discoveryv1.EndpointSlice
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "embewi-esp-svc", Namespace: "embewi"}, &eps); err != nil {
		t.Fatalf("EndpointSlice non créé : %v", err)
	}
	if len(eps.Endpoints) == 0 || len(eps.Endpoints[0].Addresses) == 0 {
		t.Fatal("EndpointSlice : aucune adresse")
	}
	if eps.Endpoints[0].Addresses[0] != "10.0.0.5" {
		t.Errorf("EndpointSlice address : got %q, want %q", eps.Endpoints[0].Addresses[0], "10.0.0.5")
	}
}

// TestReconcile_RequeuesOnHeartbeatTimeout vérifie que le résultat contient un RequeueAfter.
func TestReconcile_RequeuesAfterTimeout(t *testing.T) {
	scheme := nodeScheme(t)
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "esp-requeue", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-requeue-id"},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	r := &controller.McuNodeReconciler{Client: fc, Scheme: scheme}
	result := reconcileNode(t, r, node.Name, node.Namespace)

	if result.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter : got %v, attendu > 0 (timeout heartbeat)", result.RequeueAfter)
	}
}
