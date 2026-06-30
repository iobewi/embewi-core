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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
