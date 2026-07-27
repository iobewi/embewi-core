package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/embewi/core/internal/agent"
	"github.com/embewi/core/internal/controller"
	"github.com/embewi/core/internal/oci"
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

// preparingSetup construit un McuNode+Secret+McuDeployment en phase Preparing,
// prêts pour appeler GET /info sur le serveur TLS fourni (contrat §4, négociation
// de version d'API via api_versions).
func preparingSetup(t *testing.T, scheme *runtime.Scheme, serverURL string) (*fake.ClientBuilder, *v1alpha1.McuNode, *v1alpha1.McuDeployment) {
	t.Helper()
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "target-node", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-1"},
		Status:     v1alpha1.McuNodeStatus{State: "running", IP: strings.TrimPrefix(serverURL, "https://")},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "embewi-tokens", Namespace: "embewi"},
		Data:       map[string][]byte{"esp-1": []byte("test-token")},
	}
	dep := &v1alpha1.McuDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "prep-dep", Namespace: "embewi"},
		Spec:       v1alpha1.McuDeploymentSpec{NodeName: "target-node", Firmware: v1alpha1.FirmwareSpec{Image: "reg/fw:v1"}},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, node, secret).
		WithStatusSubresource(&v1alpha1.McuDeployment{}, &v1alpha1.McuNode{}), node, dep
}

func TestPhasePreparing_UnsupportedAPIVersion_Fails(t *testing.T) {
	scheme := deployScheme(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := agent.InfoResponse{NodeID: "esp-1", ApiVersions: []string{"v2beta1"}}
		info.Staged.State = "none"
		json.NewEncoder(w).Encode(info)
	}))
	defer ts.Close()

	builder, node, dep := preparingSetup(t, scheme, ts.URL)
	fc := builder.Build()

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhasePreparing
	depPatch.Status.BoundNode = node.Name
	depPatch.Status.DeploymentID = "fw-v1"
	depPatch.Status.Digest = "sha256:abc"
	depPatch.Status.Size = 1024
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updatedDep v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updatedDep) //nolint:errcheck
	if updatedDep.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("Phase : got %q, want Failed", updatedDep.Status.Phase)
	}
	if !strings.Contains(updatedDep.Status.Message, "APIVersionUnsupported") {
		t.Errorf("Message : got %q, want mention de APIVersionUnsupported", updatedDep.Status.Message)
	}

	var updatedNode v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, &updatedNode) //nolint:errcheck
	if updatedNode.Status.ApiVersion != "" {
		t.Errorf("McuNode.Status.ApiVersion : got %q, want vide (négociation échouée)", updatedNode.Status.ApiVersion)
	}
}

func TestPhasePreparing_APIVersionAbsent_NegotiatesV1Alpha1(t *testing.T) {
	scheme := deployScheme(t)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1alpha1/info":
			// api_versions absent → v1alpha1 supposé (device antérieur à la révision du contrat).
			info := agent.InfoResponse{NodeID: "esp-1"}
			info.Staged.State = "none"
			json.NewEncoder(w).Encode(info)
		case "/v1alpha1/ota/prepare":
			json.NewEncoder(w).Encode(agent.PrepareResponse{Accepted: true, TargetSlot: "ota_1"})
		}
	}))
	defer ts.Close()

	builder, node, dep := preparingSetup(t, scheme, ts.URL)
	fc := builder.Build()

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhasePreparing
	depPatch.Status.BoundNode = node.Name
	depPatch.Status.DeploymentID = "fw-v1"
	depPatch.Status.Digest = "sha256:abc"
	depPatch.Status.Size = 1024
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updatedDep v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updatedDep) //nolint:errcheck
	if updatedDep.Status.Phase != v1alpha1.PhaseWriting {
		t.Fatalf("Phase : got %q, want Writing (message=%q)", updatedDep.Status.Phase, updatedDep.Status.Message)
	}

	var updatedNode v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, &updatedNode) //nolint:errcheck
	if updatedNode.Status.ApiVersion != agent.SupportedAPIVersion {
		t.Errorf("McuNode.Status.ApiVersion : got %q, want %q", updatedNode.Status.ApiVersion, agent.SupportedAPIVersion)
	}
}

// TestPhasePreparing_ConfigMapMissing_FailsWithConfigMapNotFound vérifie l'écart
// d'audit 2026-07-23 : un McuConfigMap absent doit produire le reason K8s
// ConfigMapNotFound (§4b contrat), distinct de ConfigInvalid (valeur invalide).
func TestPhasePreparing_ConfigMapMissing_FailsWithConfigMapNotFound(t *testing.T) {
	scheme := deployScheme(t)
	builder, node, dep := preparingSetup(t, scheme, "https://192.0.2.1")
	dep.Spec.ConfigMapRef = "does-not-exist"
	builder = fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dep, node, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "embewi-tokens", Namespace: "embewi"},
			Data:       map[string][]byte{"esp-1": []byte("test-token")},
		}).
		WithStatusSubresource(&v1alpha1.McuDeployment{}, &v1alpha1.McuNode{})
	fc := builder.Build()

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhasePreparing
	depPatch.Status.BoundNode = node.Name
	depPatch.Status.DeploymentID = "fw-v1"
	depPatch.Status.Digest = "sha256:abc"
	depPatch.Status.Size = 1024
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updatedDep v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updatedDep) //nolint:errcheck
	if updatedDep.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("Phase : got %q, want Failed", updatedDep.Status.Phase)
	}
	if !strings.Contains(updatedDep.Status.Message, "[ConfigMapNotFound]") {
		t.Errorf("Message : got %q, want reason ConfigMapNotFound", updatedDep.Status.Message)
	}
}

// TestPhasePreparing_ConfigMapInvalidValue_FailsWithConfigInvalid vérifie que le reason
// générique ConfigInvalid reste utilisé pour les erreurs de valeur (CM présent mais
// non conforme aux limites NVS §4a), par contraste avec ConfigMapNotFound ci-dessus.
func TestPhasePreparing_ConfigMapInvalidValue_FailsWithConfigInvalid(t *testing.T) {
	scheme := deployScheme(t)
	_, node, dep := preparingSetup(t, scheme, "https://192.0.2.1")
	dep.Spec.ConfigMapRef = "bad-cm"
	cm := &v1alpha1.McuConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-cm", Namespace: "embewi"},
		Data:       map[string]string{"gpio_button_way_too_long_key": "9"},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(dep, node, cm, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "embewi-tokens", Namespace: "embewi"},
			Data:       map[string][]byte{"esp-1": []byte("test-token")},
		}).
		WithStatusSubresource(&v1alpha1.McuDeployment{}, &v1alpha1.McuNode{}).
		Build()

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhasePreparing
	depPatch.Status.BoundNode = node.Name
	depPatch.Status.DeploymentID = "fw-v1"
	depPatch.Status.Digest = "sha256:abc"
	depPatch.Status.Size = 1024
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updatedDep v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updatedDep) //nolint:errcheck
	if updatedDep.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("Phase : got %q, want Failed", updatedDep.Status.Phase)
	}
	if !strings.Contains(updatedDep.Status.Message, "[ConfigInvalid]") {
		t.Errorf("Message : got %q, want reason ConfigInvalid", updatedDep.Status.Message)
	}
}

// deployedConfigSetup construit un McuNode+Secret+McuConfigMap+McuDeployment déjà en
// phase Deployed, prêts à exercer reconcileConfigOnly (§6 contrat — idempotence config)
// contre le serveur TLS fourni.
func deployedConfigSetup(t *testing.T, scheme *runtime.Scheme, serverURL string, cmData map[string]string) (*fake.ClientBuilder, *v1alpha1.McuNode, *v1alpha1.McuDeployment) {
	t.Helper()
	now := metav1.Now()
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "target-node", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-1"},
		Status: v1alpha1.McuNodeStatus{
			State:         "running",
			IP:            strings.TrimPrefix(serverURL, "https://"),
			LastHeartbeat: &now,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "embewi-tokens", Namespace: "embewi"},
		Data:       map[string][]byte{"esp-1": []byte("test-token")},
	}
	cm := &v1alpha1.McuConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gpio-cm", Namespace: "embewi"},
		Data:       cmData,
	}
	dep := &v1alpha1.McuDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "deployed-dep", Namespace: "embewi"},
		Spec: v1alpha1.McuDeploymentSpec{
			NodeName:     "target-node",
			Firmware:     v1alpha1.FirmwareSpec{Image: "reg/fw:v1"},
			ConfigMapRef: "gpio-cm",
		},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, node, secret, cm).
		WithStatusSubresource(&v1alpha1.McuDeployment{}, &v1alpha1.McuNode{}), node, dep
}

func TestReconcileConfigOnly_GenerationsEqual_NoReboot(t *testing.T) {
	scheme := deployScheme(t)
	var rebootCalled, configPosted bool
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1alpha1/config":
			json.NewEncoder(w).Encode(agent.ConfigResponse{
				Generation: 2, ActiveGeneration: 2,
				NVS: map[string]string{"gpio_button": "9"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1alpha1/config":
			configPosted = true
			json.NewEncoder(w).Encode(map[string]any{"status": "saved"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1alpha1/reboot":
			rebootCalled = true
			json.NewEncoder(w).Encode(map[string]any{"status": "rebooting"})
		}
	}))
	defer ts.Close()

	builder, node, dep := deployedConfigSetup(t, scheme, ts.URL, map[string]string{"gpio_button": "9"})
	fc := builder.Build()

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhaseDeployed
	depPatch.Status.BoundNode = node.Name
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}
	reconcile(t, r, dep.Name, dep.Namespace)

	if configPosted {
		t.Error("POST /config : ne devait pas être appelé (nvs déjà conforme)")
	}
	if rebootCalled {
		t.Error("POST /reboot : ne devait pas être appelé (active_generation == generation)")
	}
}

// TestReconcileConfigOnly_ActiveGenerationBehind_RebootsWithoutRepush couvre l'écart
// d'audit 2026-07-23 : un crash Core entre POST /config et POST /reboot laissait le
// device durablement en config poussée-non-appliquée, jamais détecté (§6 contrat).
func TestReconcileConfigOnly_ActiveGenerationBehind_RebootsWithoutRepush(t *testing.T) {
	scheme := deployScheme(t)
	var rebootCalled, configPosted bool
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1alpha1/config":
			// nvs déjà conforme au désiré, mais generation > active_generation :
			// le POST /config précédent a réussi, le reboot qui devait suivre non.
			json.NewEncoder(w).Encode(agent.ConfigResponse{
				Generation: 3, ActiveGeneration: 2,
				NVS: map[string]string{"gpio_button": "9"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1alpha1/config":
			configPosted = true
			json.NewEncoder(w).Encode(map[string]any{"status": "saved"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1alpha1/reboot":
			rebootCalled = true
			json.NewEncoder(w).Encode(map[string]any{"status": "rebooting"})
		}
	}))
	defer ts.Close()

	builder, node, dep := deployedConfigSetup(t, scheme, ts.URL, map[string]string{"gpio_button": "9"})
	fc := builder.Build()

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhaseDeployed
	depPatch.Status.BoundNode = node.Name
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}
	reconcile(t, r, dep.Name, dep.Namespace)

	if configPosted {
		t.Error("POST /config : ne devait pas être ré-appelé (nvs déjà conforme, seul le reboot manquait)")
	}
	if !rebootCalled {
		t.Error("POST /reboot : devait être appelé (active_generation < generation, reboot manqué détecté)")
	}
}

func TestReconcileConfigOnly_NVSDiverges_PushesAndReboots(t *testing.T) {
	scheme := deployScheme(t)
	var rebootCalled, configPosted bool
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1alpha1/config":
			json.NewEncoder(w).Encode(agent.ConfigResponse{
				Generation: 1, ActiveGeneration: 1,
				NVS: map[string]string{"gpio_button": "8"}, // diverge du désiré "9"
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1alpha1/config":
			configPosted = true
			json.NewEncoder(w).Encode(map[string]any{"status": "saved"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1alpha1/reboot":
			rebootCalled = true
			json.NewEncoder(w).Encode(map[string]any{"status": "rebooting"})
		}
	}))
	defer ts.Close()

	builder, node, dep := deployedConfigSetup(t, scheme, ts.URL, map[string]string{"gpio_button": "9"})
	fc := builder.Build()

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhaseDeployed
	depPatch.Status.BoundNode = node.Name
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}
	reconcile(t, r, dep.Name, dep.Namespace)

	if !configPosted {
		t.Error("POST /config : devait être appelé (nvs divergeait du désiré)")
	}
	if !rebootCalled {
		t.Error("POST /reboot : devait être appelé après le push")
	}
}

// TestPhaseWriting_OCIBlobDigestMismatch_DoesNotAdvanceToActivating couvre le re-hash du
// blob streamé (§1/§9 contrat) : même si l'agent accepte l'écriture, un digest divergent
// détecté après coup (registre compromis / blob substitué) ne doit pas faire avancer le
// déploiement vers Activating.
func TestPhaseWriting_OCIBlobDigestMismatch_DoesNotAdvanceToActivating(t *testing.T) {
	scheme := deployScheme(t)

	content := []byte("firmware binaire de test")
	ociServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content) //nolint:errcheck
	}))
	defer ociServer.Close()

	deviceServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/v1alpha1/ota/write" {
			json.NewEncoder(w).Encode(map[string]any{"status": "written"}) //nolint:errcheck
		}
	}))
	defer deviceServer.Close()

	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "target-node", Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: "esp-1"},
		Status:     v1alpha1.McuNodeStatus{State: "running", IP: strings.TrimPrefix(deviceServer.URL, "https://")},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "embewi-tokens", Namespace: "embewi"},
		Data:       map[string][]byte{"esp-1": []byte("test-token")},
	}
	// digest délibérément faux (ne correspond pas au contenu réellement servi par ociServer).
	wrongDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	dep := &v1alpha1.McuDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "write-dep", Namespace: "embewi"},
		Spec: v1alpha1.McuDeploymentSpec{
			NodeName: "target-node",
			Firmware: v1alpha1.FirmwareSpec{Image: strings.TrimPrefix(ociServer.URL, "http://") + "/repo:v1"},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, node, secret).
		WithStatusSubresource(&v1alpha1.McuDeployment{}, &v1alpha1.McuNode{}).
		Build()

	depPatch := dep.DeepCopy()
	depPatch.Status.Phase = v1alpha1.PhaseWriting
	depPatch.Status.BoundNode = node.Name
	depPatch.Status.DeploymentID = "fw-v1"
	depPatch.Status.Digest = wrongDigest
	depPatch.Status.Size = int64(len(content))
	fc.Status().Update(context.Background(), depPatch) //nolint:errcheck

	r := &controller.McuDeploymentReconciler{
		Client:      fc,
		Scheme:      scheme,
		OCI:         oci.New(),
		TokenSecret: "embewi-tokens",
		Recorder:    record.NewFakeRecorder(10),
	}

	reconcile(t, r, dep.Name, dep.Namespace)

	var updatedDep v1alpha1.McuDeployment
	fc.Get(context.Background(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, &updatedDep) //nolint:errcheck
	if updatedDep.Status.Phase != v1alpha1.PhaseWriting {
		t.Errorf("Phase : got %q, want Writing (ne doit pas avancer sur digest mismatch)", updatedDep.Status.Phase)
	}

	rec := r.Recorder.(*record.FakeRecorder)
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "OTABlobDigestMismatch") {
			t.Errorf("event reçu ne contient pas OTABlobDigestMismatch : %q", ev)
		}
	default:
		t.Error("aucun event OTABlobDigestMismatch émis")
	}
}
