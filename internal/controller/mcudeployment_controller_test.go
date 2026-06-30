package controller_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/embewi/core/api/v1alpha1"
	"github.com/embewi/core/internal/controller"
)

func deployScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// newDep crée un McuDeployment minimal avec nodeName.
func newDep(name, namespace, nodeName string) *v1alpha1.McuDeployment {
	return &v1alpha1.McuDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.McuDeploymentSpec{
			NodeName: nodeName,
			Firmware: v1alpha1.FirmwareSpec{Image: "registry.local/fw:v1.0.0"},
		},
	}
}

// reconcile effectue un appel Reconcile et retourne le result.
func reconcile(t *testing.T, r *controller.McuDeploymentReconciler, name, ns string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: ns},
	})
	if err != nil {
		t.Fatalf("Reconcile error : %v", err)
	}
	return result
}

func TestPhaseBinding_NoDevice(t *testing.T) {
	scheme := deployScheme(t)
	dep := newDep("my-dep", "embewi", "nonexistent-node")

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep).
		WithStatusSubresource(&v1alpha1.McuDeployment{}).
		Build()

	r := &controller.McuDeploymentReconciler{
		Client:   fc,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updated v1alpha1.McuDeployment
	if err := fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != v1alpha1.PhaseFailed {
		t.Errorf("Phase : got %q, want %q", updated.Status.Phase, v1alpha1.PhaseFailed)
	}
	if updated.Status.Message == "" {
		t.Error("Message : attendu non-vide pour un échec")
	}
}

func TestPhaseBinding_ExplicitNodeName_AdvancesToPulling(t *testing.T) {
	scheme := deployScheme(t)
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "target-node", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "embewi-abc"},
	}
	dep := newDep("my-dep", "embewi", "target-node")

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep, node).
		WithStatusSubresource(&v1alpha1.McuDeployment{}).
		Build()

	r := &controller.McuDeploymentReconciler{
		Client:   fc,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updated v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updated) //nolint:errcheck
	if updated.Status.Phase != v1alpha1.PhasePulling {
		t.Errorf("Phase : got %q, want %q", updated.Status.Phase, v1alpha1.PhasePulling)
	}
	if updated.Status.BoundNode != "target-node" {
		t.Errorf("BoundNode : got %q, want %q", updated.Status.BoundNode, "target-node")
	}
}

func TestPhaseBinding_AmbiguousDevices(t *testing.T) {
	scheme := deployScheme(t)
	// Deux nodes avec le même label → ambiguïté.
	node1 := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "embewi", Labels: map[string]string{"role": "wheel"}},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-1"},
	}
	node2 := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2", Namespace: "embewi", Labels: map[string]string{"role": "wheel"}},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-2"},
	}
	dep := &v1alpha1.McuDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-dep", Namespace: "embewi"},
		Spec: v1alpha1.McuDeploymentSpec{
			NodeSelector: map[string]string{"role": "wheel"},
			Firmware:     v1alpha1.FirmwareSpec{Image: "registry.local/fw:v1.0.0"},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep, node1, node2).
		WithStatusSubresource(&v1alpha1.McuDeployment{}).
		Build()

	r := &controller.McuDeploymentReconciler{
		Client:   fc,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updated v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updated) //nolint:errcheck
	if updated.Status.Phase != v1alpha1.PhaseFailed {
		t.Errorf("Phase : got %q, want %q (ambiguïté doit échouer)", updated.Status.Phase, v1alpha1.PhaseFailed)
	}
}

func TestPhaseBinding_DeviceBusy(t *testing.T) {
	scheme := deployScheme(t)
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "target-node", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-1"},
	}
	// McuDeployment déjà bindé sur ce node, en cours (phase Writing).
	existing := &v1alpha1.McuDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-dep", Namespace: "embewi"},
		Spec:       v1alpha1.McuDeploymentSpec{NodeName: "target-node", Firmware: v1alpha1.FirmwareSpec{Image: "reg/fw:v1"}},
	}
	dep := newDep("new-dep", "embewi", "target-node")

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep, node, existing).
		WithStatusSubresource(&v1alpha1.McuDeployment{}, &v1alpha1.McuNode{}).
		Build()

	// Mettre existing en phase Writing (pas Deployed/Failed).
	existingPatch := existing.DeepCopy()
	existingPatch.Status.Phase = v1alpha1.PhaseWriting
	existingPatch.Status.BoundNode = "target-node"
	fc.Status().Update(context.Background(), existingPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:   fc,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updated v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updated) //nolint:errcheck
	if updated.Status.Phase != v1alpha1.PhaseFailed {
		t.Errorf("Phase : got %q, want Failed (device busy)", updated.Status.Phase)
	}
}

func TestPhaseDeployed_NoConfigMapRef_IsTerminal(t *testing.T) {
	scheme := deployScheme(t)
	now := metav1.Now()
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "done-node", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "done-esp"},
	}
	dep := &v1alpha1.McuDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "done-dep", Namespace: "embewi"},
		Spec:       v1alpha1.McuDeploymentSpec{Firmware: v1alpha1.FirmwareSpec{Image: "reg/fw:v1"}},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep, node).
		WithStatusSubresource(&v1alpha1.McuDeployment{}, &v1alpha1.McuNode{}).
		Build()

	// Forcer la phase Deployed avec BoundNode et heartbeat récent.
	nodePatch := node.DeepCopy()
	nodePatch.Status.LastHeartbeat = &now
	nodePatch.Status.State = "running"
	fc.Status().Update(context.Background(), nodePatch) //nolint:errcheck

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhaseDeployed
	depPatch.Status.BoundNode = "done-node"
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:   fc,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}

	result := reconcile(t, r, dep.Name, dep.Namespace)
	// Sans configMapRef, Deployed ne requeue pas après la mise à jour des conditions.
	if result.Requeue || result.RequeueAfter > 0 {
		t.Errorf("Deployed terminal : attendu pas de requeue, got %+v", result)
	}
}

func TestConditions_PullingPhase_ProgressingTrue(t *testing.T) {
	scheme := deployScheme(t)
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "target-node", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-1"},
	}
	dep := newDep("my-dep", "embewi", "target-node")

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep, node).
		WithStatusSubresource(&v1alpha1.McuDeployment{}).
		Build()

	r := &controller.McuDeploymentReconciler{
		Client:   fc,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updated v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updated) //nolint:errcheck

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, "Progressing")
	if cond == nil {
		t.Fatal("condition Progressing absente")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("Progressing status : got %q, want True", cond.Status)
	}
	if cond.Reason != "OTAInProgress" {
		t.Errorf("Progressing reason : got %q, want OTAInProgress", cond.Reason)
	}
}

// TestPhaseConfirming_Timeout vérifie que le timeout négatif déclenche PhaseFailed.
func TestPhaseConfirming_Timeout(t *testing.T) {
	scheme := deployScheme(t)
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "target-node", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-1"},
		Status:     v1alpha1.McuNodeStatus{State: "pending_verify", IP: "192.168.1.1"},
	}
	// Secret token requis par nodeClient.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "embewi-tokens", Namespace: "embewi"},
		Data:       map[string][]byte{"esp-1": []byte("test-token")},
	}
	dep := &v1alpha1.McuDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "confirm-dep",
			Namespace: "embewi",
			Annotations: map[string]string{
				// Timestamp expiré — plus de 2 minutes dans le passé.
				"embewi.io/confirming-since": time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339),
			},
		},
		Spec: v1alpha1.McuDeploymentSpec{NodeName: "target-node", Firmware: v1alpha1.FirmwareSpec{Image: "reg/fw:v1"}},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep, node, secret).
		WithStatusSubresource(&v1alpha1.McuDeployment{}, &v1alpha1.McuNode{}).
		Build()

	// Forcer la phase Confirming + BoundNode (le status du node est déjà dans WithObjects).
	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhaseConfirming
	depPatch.Status.BoundNode = "target-node"
	depPatch.Status.DeploymentID = "fw-v1"
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updated v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updated) //nolint:errcheck
	if updated.Status.Phase != v1alpha1.PhaseFailed {
		t.Errorf("Phase après timeout : got %q, want Failed", updated.Status.Phase)
	}
}
