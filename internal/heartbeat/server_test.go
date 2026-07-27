package heartbeat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/embewi/core/api/v1alpha1"
	"github.com/embewi/core/internal/heartbeat"
)

func testScheme(t *testing.T) *runtime.Scheme {
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

func newNode(name, nodeID string) *v1alpha1.McuNode {
	return &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "embewi"},
		Spec:       v1alpha1.McuNodeSpec{NodeID: nodeID},
	}
}

func postHB(t *testing.T, ts *httptest.Server, payload interface{}) *http.Response {
	t.Helper()
	return postHBAuth(t, ts, payload, "")
}

func postHBAuth(t *testing.T, ts *httptest.Server, payload interface{}, token string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1alpha1/heartbeat", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest : %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST heartbeat : %v", err)
	}
	return resp
}

// tokenSecret crée un corev1.Secret de test contenant le token Bearer d'un node.
func tokenSecret(nodeID, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "embewi-tokens", Namespace: "embewi"},
		Data:       map[string][]byte{nodeID: []byte(token)},
	}
}

// TestHandleHeartbeat_UpdatesNodeStatus vérifie que le status du McuNode est mis à jour.
func TestHandleHeartbeat_UpdatesNodeStatus(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("embewi-abc", "embewi-abc123")
	secret := tokenSecret("embewi-abc123", "tok-abc")

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{
		NodeID:           "embewi-abc123",
		IP:               "192.168.1.100",
		State:            "running",
		OtaValidated:     true,
		HeapFree:         82344,
		RSSI:             -61,
		ConfigGeneration: 2,
	}, "tok-abc")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status : got %d, want 200", resp.StatusCode)
	}

	var updated v1alpha1.McuNode
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "embewi-abc", Namespace: "embewi"}, &updated); err != nil {
		t.Fatalf("Get McuNode : %v", err)
	}
	if updated.Status.IP != "192.168.1.100" {
		t.Errorf("IP : got %q, want %q", updated.Status.IP, "192.168.1.100")
	}
	if updated.Status.State != "running" {
		t.Errorf("State : got %q, want %q", updated.Status.State, "running")
	}
	if !updated.Status.OtaValidated {
		t.Error("OtaValidated : got false, want true")
	}
	if updated.Status.HeapFree != 82344 {
		t.Errorf("HeapFree : got %d, want 82344", updated.Status.HeapFree)
	}
	if updated.Status.ConfigGeneration != 2 {
		t.Errorf("ConfigGeneration : got %d, want 2", updated.Status.ConfigGeneration)
	}
	if updated.Status.LastHeartbeat == nil {
		t.Error("LastHeartbeat : attendu non-nil")
	}
	if !updated.Status.Ready {
		t.Error("Ready : got false, want true (state=running + ota_validated=true)")
	}
}

// conflictOnceClient simule une écriture concurrente (ex: le reconciler McuNode
// qui patch le même Status en parallèle sur timeout heartbeat) entre le Get et
// le Patch du handler heartbeat : au premier appel à Status().Patch(), une autre
// mutation est appliquée sous le capot, forçant un conflit de resourceVersion
// sur la tentative en cours et vérifiant que le retry ré-applique correctement
// le heartbeat plutôt que d'écraser silencieusement (ou de perdre) l'un des deux écrits.
type conflictOnceClient struct {
	client.Client
	key       types.NamespacedName
	triggered bool
}

func (c *conflictOnceClient) Status() client.SubResourceWriter {
	return &conflictOnceStatusWriter{SubResourceWriter: c.Client.Status(), outer: c}
}

type conflictOnceStatusWriter struct {
	client.SubResourceWriter
	outer *conflictOnceClient
}

func (w *conflictOnceStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if !w.outer.triggered {
		w.outer.triggered = true
		var concurrent v1alpha1.McuNode
		if err := w.outer.Client.Get(ctx, w.outer.key, &concurrent); err != nil {
			return err
		}
		p := client.MergeFromWithOptions(concurrent.DeepCopy(), client.MergeFromWithOptimisticLock{})
		concurrent.Status.State = "offline" // ce que le reconciler McuNode aurait posé sur timeout
		if err := w.outer.Client.Status().Patch(ctx, &concurrent, p); err != nil {
			return err
		}
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// TestHandleHeartbeat_ConcurrentWrite_RetriesAndWins vérifie que le heartbeat
// ne se fait pas silencieusement écraser par une écriture concurrente : le
// conflit de resourceVersion doit déclencher un retry qui repart d'une base
// fraîche, et le heartbeat (plus récent) doit finir par gagner.
func TestHandleHeartbeat_ConcurrentWrite_RetriesAndWins(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("esp-race", "esp-race-id")
	secret := tokenSecret("esp-race-id", "tok-race")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	wrapped := &conflictOnceClient{Client: fc, key: types.NamespacedName{Name: "esp-race", Namespace: "embewi"}}
	srv := heartbeat.New(":0", wrapped)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{
		NodeID:           "esp-race-id",
		IP:               "10.0.0.9",
		State:            "running",
		OtaValidated:     true,
		ConfigGeneration: 1,
	}, "tok-race")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status : got %d, want 200", resp.StatusCode)
	}
	if !wrapped.triggered {
		t.Fatal("le conflit simulé n'a jamais été déclenché — le test ne prouve rien")
	}

	var updated v1alpha1.McuNode
	if err := fc.Get(context.Background(), wrapped.key, &updated); err != nil {
		t.Fatalf("Get McuNode : %v", err)
	}
	if updated.Status.State != "running" {
		t.Errorf("State : got %q, want %q (le heartbeat doit gagner après retry, pas rester écrasé à offline)", updated.Status.State, "running")
	}
	if !updated.Status.Ready {
		t.Error("Ready : got false, want true — le retry doit réappliquer le heartbeat complet, pas seulement state")
	}
}

// TestHandleHeartbeat_NodeNotFound_Returns200 vérifie que l'endpoint retourne 200
// même si le McuNode n'existe pas encore (l'agent ne doit pas crasher).
func TestHandleHeartbeat_NodeNotFound_Returns200(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postHB(t, ts, heartbeat.HeartbeatPayload{NodeID: "inconnu"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status : got %d, want 200 (doit absorber les nodes inconnus)", resp.StatusCode)
	}
}

// TestHandleHeartbeat_DuplicateNodeID_Returns200AndSkipsUpdate vérifie l'écart d'audit
// 2026-07-23 (§1a contrat) : deux McuNode déclarant le même spec.nodeId ne doivent pas
// voir le heartbeat attaché silencieusement au premier trouvé — ni l'un ni l'autre ne
// doit être mis à jour, et un Event DuplicateNodeID doit être émis sur chacun.
func TestHandleHeartbeat_DuplicateNodeID_Returns200AndSkipsUpdate(t *testing.T) {
	scheme := testScheme(t)
	nodeA := newNode("esp-dup-a", "esp-dup-id")
	nodeB := newNode("esp-dup-b", "esp-dup-id")
	secret := tokenSecret("esp-dup-id", "tok-dup")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nodeA, nodeB, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	rec := record.NewFakeRecorder(10)
	srv.Recorder = rec
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{NodeID: "esp-dup-id", State: "running"}, "tok-dup")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status : got %d, want 200 (fail-safe, agent ne doit pas crasher)", resp.StatusCode)
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-rec.Events:
			if !strings.Contains(ev, "DuplicateNodeID") {
				t.Errorf("event reçu ne contient pas DuplicateNodeID : %q", ev)
			}
			seen[ev] = true
		default:
			t.Errorf("event DuplicateNodeID manquant (attendu 2, reçu %d)", i)
		}
	}

	var a, b v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-dup-a", Namespace: "embewi"}, &a) //nolint:errcheck
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-dup-b", Namespace: "embewi"}, &b) //nolint:errcheck
	if a.Status.State != "" || b.Status.State != "" {
		t.Errorf("aucun des deux McuNode ne doit être mis à jour : a.State=%q b.State=%q", a.Status.State, b.Status.State)
	}
}

// TestHandleHeartbeat_TempFilter vérifie que la sentinelle -127.0 n'est pas écrite.
func TestHandleHeartbeat_TempFilter(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("esp-temp", "esp-temp-id")
	secret := tokenSecret("esp-temp-id", "tok-temp")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Premier heartbeat : temp valide.
	postHBAuth(t, ts, heartbeat.HeartbeatPayload{NodeID: "esp-temp-id", State: "running", TempCelsius: 41.5}, "tok-temp")

	var n v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-temp", Namespace: "embewi"}, &n) //nolint:errcheck
	if n.Status.TempCelsius != 41.5 {
		t.Errorf("TempCelsius après valide : got %v, want 41.5", n.Status.TempCelsius)
	}

	// Second heartbeat : sentinelle -127.0 — la valeur ne doit pas changer.
	postHBAuth(t, ts, heartbeat.HeartbeatPayload{NodeID: "esp-temp-id", State: "running", TempCelsius: -127.0}, "tok-temp")
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-temp", Namespace: "embewi"}, &n) //nolint:errcheck
	if n.Status.TempCelsius != 41.5 {
		t.Errorf("TempCelsius après sentinelle : got %v, want 41.5 (doit rester inchangé)", n.Status.TempCelsius)
	}
}

// TestHandleHeartbeat_IPFallback vérifie le fallback sur RemoteAddr quand ip est vide.
func TestHandleHeartbeat_IPFallback(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("esp-ip", "esp-ip-id")
	secret := tokenSecret("esp-ip-id", "tok-ip")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Heartbeat sans champ ip → fallback sur RemoteAddr (127.0.0.1 en test).
	postHBAuth(t, ts, heartbeat.HeartbeatPayload{NodeID: "esp-ip-id", State: "running", IP: ""}, "tok-ip")

	var n v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-ip", Namespace: "embewi"}, &n) //nolint:errcheck
	if n.Status.IP == "" {
		t.Error("IP : attendu non-vide (fallback RemoteAddr), got empty")
	}
	if n.Status.IP == "192.168.1.100" {
		t.Error("IP : ne doit pas provenir du payload vide")
	}
}

// TestHandleHeartbeat_IPMismatch_EmitsEvent vérifie que heartbeat.ip != IP source TCP
// émet un Event HeartbeatIPMismatch (§1/§8 contrat) sans bloquer la requête ni empêcher
// heartbeat.ip de rester la source de vérité pour l'EndpointSlice (détection, pas rejet).
func TestHandleHeartbeat_IPMismatch_EmitsEvent(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("esp-mismatch", "esp-mismatch-id")
	secret := tokenSecret("esp-mismatch-id", "tok-mismatch")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	rec := record.NewFakeRecorder(10)
	srv.Recorder = rec
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// httptest sert sur 127.0.0.1 : une ip déclarée différente est donc une divergence.
	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{NodeID: "esp-mismatch-id", State: "running", IP: "192.168.10.42"}, "tok-mismatch")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("divergence ip : got %d, want 200 (détection, pas rejet)", resp.StatusCode)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "HeartbeatIPMismatch") {
			t.Errorf("event reçu ne contient pas HeartbeatIPMismatch : %q", ev)
		}
	default:
		t.Error("aucun event HeartbeatIPMismatch émis")
	}

	var n v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-mismatch", Namespace: "embewi"}, &n) //nolint:errcheck
	if n.Status.IP != "192.168.10.42" {
		t.Errorf("Status.IP : got %q, want heartbeat.ip malgré la divergence (pas de blocage)", n.Status.IP)
	}
}

// TestHandleHeartbeat_PreviousToken_Accepted vérifie qu'un heartbeat authentifié
// avec previousToken est accepté pendant une fenêtre de rotation (§4 contrat) —
// ce n'est pas une anomalie tant que le device n'a pas confirmé le nouveau token.
func TestHandleHeartbeat_PreviousToken_Accepted(t *testing.T) {
	scheme := testScheme(t)
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "esp-rot", Namespace: "embewi"},
		Spec: v1alpha1.McuNodeSpec{
			NodeID:   "esp-rot-id",
			TokenRef: v1alpha1.SecretRef{Name: "esp-rot-token"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "esp-rot-token", Namespace: "embewi"},
		Data: map[string][]byte{
			"token":         []byte("new-token"),
			"previousToken": []byte("old-token"),
		},
	}
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Le device n'a pas encore reçu newToken : il continue d'authentifier avec
	// old-token — doit être accepté, pas seulement toléré silencieusement.
	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{NodeID: "esp-rot-id", State: "running"}, "old-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("heartbeat sur previousToken : got %d, want 200", resp.StatusCode)
	}

	// previousToken reste en place : ce n'est pas ce heartbeat qui confirme la
	// rotation (il faudrait un heartbeat authentifié sur newToken pour ça).
	var sec corev1.Secret
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-rot-token", Namespace: "embewi"}, &sec) //nolint:errcheck
	if string(sec.Data["previousToken"]) != "old-token" {
		t.Error("previousToken : ne doit pas être effacé par un heartbeat authentifié dessus")
	}
}

// TestHandleHeartbeat_CurrentToken_ClearsPreviousToken vérifie que le premier
// heartbeat authentifié avec le nouveau token efface previousToken (§4 contrat,
// confirmation que la rotation a abouti côté device).
func TestHandleHeartbeat_CurrentToken_ClearsPreviousToken(t *testing.T) {
	scheme := testScheme(t)
	node := &v1alpha1.McuNode{
		ObjectMeta: metav1.ObjectMeta{Name: "esp-rot2", Namespace: "embewi"},
		Spec: v1alpha1.McuNodeSpec{
			NodeID:   "esp-rot2-id",
			TokenRef: v1alpha1.SecretRef{Name: "esp-rot2-token"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "esp-rot2-token", Namespace: "embewi"},
		Data: map[string][]byte{
			"token":         []byte("new-token"),
			"previousToken": []byte("old-token"),
		},
	}
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{NodeID: "esp-rot2-id", State: "running"}, "new-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("heartbeat sur newToken : got %d, want 200", resp.StatusCode)
	}

	var sec corev1.Secret
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-rot2-token", Namespace: "embewi"}, &sec) //nolint:errcheck
	if _, ok := sec.Data["previousToken"]; ok {
		t.Error("previousToken : doit être effacé après confirmation par heartbeat sur newToken")
	}
	if string(sec.Data["token"]) != "new-token" {
		t.Error("token : ne doit pas être altéré par l'effacement de previousToken")
	}
}

// TestHandleHeartbeat_ClockUnsynced_NotReady vérifie qu'un heartbeat portant
// reason:"clock_unsynced" (§5 contrat, canal de détresse NTP) n'est jamais promu
// Ready=True, même si state=running et ota_validated=true par ailleurs — c'est un
// signal diagnostique, pas une confirmation opérationnelle complète.
func TestHandleHeartbeat_ClockUnsynced_NotReady(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("esp-clock", "esp-clock-id")
	secret := tokenSecret("esp-clock-id", "tok-clock")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{
		NodeID:       "esp-clock-id",
		State:        "running",
		OtaValidated: true,
		TS:           0,
		Reason:       "clock_unsynced",
	}, "tok-clock")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status : got %d, want 200 (accepté, pas rejeté)", resp.StatusCode)
	}

	var n v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-clock", Namespace: "embewi"}, &n) //nolint:errcheck
	if n.Status.Ready {
		t.Error("Ready : ne doit jamais être true sur un heartbeat clock_unsynced, même state=running+ota_validated=true")
	}
	cond := apimeta.FindStatusCondition(n.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "ClockUnsynced" {
		t.Errorf("condition Ready : got %+v, want reason=ClockUnsynced", cond)
	}
	// LastHeartbeat doit quand même être mis à jour — le device est vivant, silence
	// évité (§2 contrat, règle d'or) malgré le canal dégradé.
	if n.Status.LastHeartbeat == nil {
		t.Error("LastHeartbeat : doit être mis à jour même en clock_unsynced (règle d'or §2)")
	}
}

// TestHandleHeartbeat_NoReason_StillReady vérifie qu'un heartbeat normal (sans
// champ reason) n'est pas affecté par la logique clock_unsynced — non-régression.
func TestHandleHeartbeat_NoReason_StillReady(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("esp-noreason", "esp-noreason-id")
	secret := tokenSecret("esp-noreason-id", "tok-noreason")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{
		NodeID:       "esp-noreason-id",
		State:        "running",
		OtaValidated: true,
	}, "tok-noreason")
	defer resp.Body.Close()

	var n v1alpha1.McuNode
	fc.Get(context.Background(), types.NamespacedName{Name: "esp-noreason", Namespace: "embewi"}, &n) //nolint:errcheck
	if !n.Status.Ready {
		t.Error("Ready : doit être true (state=running, ota_validated=true, pas de reason)")
	}
}

// TestHandleHeartbeat_InvalidToken vérifie que un mauvais Bearer token retourne 401.
func TestHandleHeartbeat_InvalidToken(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("esp-auth", "esp-auth-id")
	secret := tokenSecret("esp-auth-id", "correct-token")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postHBAuth(t, ts, heartbeat.HeartbeatPayload{NodeID: "esp-auth-id", State: "running"}, "wrong-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("token invalide : got %d, want 401", resp.StatusCode)
	}
}

// TestHandleHeartbeat_MethodNotAllowed vérifie le rejet des requêtes non-POST.
func TestHandleHeartbeat_MethodNotAllowed(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/v1alpha1/heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status : got %d, want 405", resp.StatusCode)
	}
}

// TestHandleLogWS_AuthAndStream vérifie que le flux WS est authentifié sur le premier
// frame et que les entrées de log sont acceptées après auth.
func TestHandleLogWS_AuthAndStream(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("ws-node", "ws-node-id")
	secret := tokenSecret("ws-node-id", "ws-token")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Convertir l'URL http → ws.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1alpha1/logs"
	header := http.Header{"Authorization": {"Bearer ws-token"}}
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("WS dial : %v (HTTP %v)", err, resp)
	}
	defer conn.Close()

	// Premier frame : authentifie + logue.
	frame, _ := json.Marshal(heartbeat.LogPayload{
		TS: 1719392051, Node: "ws-node-id", Workload: "test-wl", Level: "info", Msg: "hello ws",
	})
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("WriteMessage : %v", err)
	}

	// Second frame : déjà authentifié.
	frame2, _ := json.Marshal(heartbeat.LogPayload{
		TS: 1719392052, Node: "ws-node-id", Workload: "test-wl", Level: "error", Msg: "oops",
	})
	if err := conn.WriteMessage(websocket.TextMessage, frame2); err != nil {
		t.Fatalf("WriteMessage 2 : %v", err)
	}

	// Fermeture propre côté client.
	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

// TestHandleLogWS_InvalidToken vérifie que la connexion est coupée si le token est invalide.
func TestHandleLogWS_InvalidToken(t *testing.T) {
	scheme := testScheme(t)
	node := newNode("ws-auth-node", "ws-auth-id")
	secret := tokenSecret("ws-auth-id", "correct-ws-token")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(&v1alpha1.McuNode{}).
		Build()

	srv := heartbeat.New(":0", fc)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1alpha1/logs"
	header := http.Header{"Authorization": {"Bearer wrong-token"}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("WS dial : %v", err)
	}
	defer conn.Close()

	// Envoyer un frame — le serveur doit fermer avec 1008 (Policy Violation).
	frame, _ := json.Marshal(heartbeat.LogPayload{Node: "ws-auth-id", Level: "info", Msg: "test"})
	_ = conn.WriteMessage(websocket.TextMessage, frame)

	// Lire la réponse — doit être un close frame.
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Error("attendu une fermeture WS, connexion encore ouverte")
		return
	}
	ce, ok := err.(*websocket.CloseError)
	if !ok || ce.Code != websocket.ClosePolicyViolation {
		t.Errorf("close code : got %v, want 1008 ClosePolicyViolation", err)
	}
}
