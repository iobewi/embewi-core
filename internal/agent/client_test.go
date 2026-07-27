package agent_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/embewi/core/internal/agent"
)

// otaChunkSize reflète la constante non exportée agent.otaChunkSize (64 KiB) — les tests
// de chunking ci-dessous en dépendent pour construire des firmwares de test multi-plages.
const otaChunkSize = 64 * 1024

// tlsClient crée un agent.Client pointant sur un httptest.TLSServer.
// InsecureSkipVerify=true dans le client permet d'accepter le cert auto-signé de httptest.
func tlsClient(t *testing.T, h http.HandlerFunc) *agent.Client {
	t.Helper()
	ts := httptest.NewTLSServer(h)
	t.Cleanup(ts.Close)
	return agent.New(strings.TrimPrefix(ts.URL, "https://"), "test-token")
}

func assertReq(t *testing.T, r *http.Request, method, path string) {
	t.Helper()
	if r.Method != method {
		t.Errorf("méthode : got %q, want %q", r.Method, method)
	}
	if r.URL.Path != path {
		t.Errorf("path : got %q, want %q", r.URL.Path, path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization : got %q, want %q", got, "Bearer test-token")
	}
}

func TestGetInfo(t *testing.T) {
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodGet, "/v1alpha1/info")
		json.NewEncoder(w).Encode(agent.InfoResponse{
			NodeID:      "esp32-abc",
			Chip:        "esp32s3",
			AppPort:     8080,
			ApiVersions: []string{"v1alpha1"},
		})
	})

	info, err := cli.GetInfo()
	if err != nil {
		t.Fatalf("GetInfo : %v", err)
	}
	if info.NodeID != "esp32-abc" {
		t.Errorf("NodeID : got %q, want %q", info.NodeID, "esp32-abc")
	}
	if info.AppPort != 8080 {
		t.Errorf("AppPort : got %d, want 8080", info.AppPort)
	}
	if len(info.ApiVersions) != 1 || info.ApiVersions[0] != "v1alpha1" {
		t.Errorf("ApiVersions : got %v, want [v1alpha1]", info.ApiVersions)
	}
}

func TestNegotiateAPIVersion_FieldAbsent_AssumesV1Alpha1(t *testing.T) {
	got, err := agent.NegotiateAPIVersion(nil)
	if err != nil {
		t.Fatalf("NegotiateAPIVersion(nil) : %v", err)
	}
	if got != agent.SupportedAPIVersion {
		t.Errorf("got %q, want %q", got, agent.SupportedAPIVersion)
	}
}

func TestNegotiateAPIVersion_CommonVersion_Picked(t *testing.T) {
	got, err := agent.NegotiateAPIVersion([]string{"v1alpha1"})
	if err != nil {
		t.Fatalf("NegotiateAPIVersion : %v", err)
	}
	if got != agent.SupportedAPIVersion {
		t.Errorf("got %q, want %q", got, agent.SupportedAPIVersion)
	}
}

func TestNegotiateAPIVersion_NoCommonVersion_Errors(t *testing.T) {
	_, err := agent.NegotiateAPIVersion([]string{"v2beta1"})
	if !errors.Is(err, agent.ErrUnsupportedAPIVersion) {
		t.Fatalf("got err %v, want ErrUnsupportedAPIVersion", err)
	}
}

func TestOTAWrite_Written(t *testing.T) {
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodPut, "/v1alpha1/ota/write")
		if got := r.Header.Get("X-Embewi-Deployment-Id"); got != "deploy-1" {
			t.Errorf("X-Embewi-Deployment-Id : got %q, want %q", got, "deploy-1")
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "written"})
	})

	err := cli.OTAWrite("deploy-1", "sha256:abc", 13, strings.NewReader("fake-firmware"))
	if err != nil {
		t.Fatalf("OTAWrite succès : attendu nil, got %v", err)
	}
}

func TestOTAWrite_DigestMismatch(t *testing.T) {
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "digest_mismatch"})
	})

	err := cli.OTAWrite("deploy-1", "sha256:abc", 4, strings.NewReader("data"))
	var writeErr *agent.OTAWriteError
	if !errors.As(err, &writeErr) {
		t.Fatalf("attendu *OTAWriteError, got %T : %v", err, err)
	}
	if writeErr.Status != "digest_mismatch" {
		t.Errorf("Status : got %q, want %q", writeErr.Status, "digest_mismatch")
	}
	if writeErr.HTTPStatus != 0 {
		t.Errorf("HTTPStatus : got %d, want 0 (erreur métier HTTP 200)", writeErr.HTTPStatus)
	}
}

func TestOTAWrite_BeginFailed_HTTP500(t *testing.T) {
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"ota_begin_failed"}`)) //nolint:errcheck
	})

	err := cli.OTAWrite("deploy-1", "sha256:abc", 4, strings.NewReader("data"))
	var writeErr *agent.OTAWriteError
	if !errors.As(err, &writeErr) {
		t.Fatalf("attendu *OTAWriteError, got %T : %v", err, err)
	}
	if writeErr.Status != "ota_begin_failed" {
		t.Errorf("Status : got %q, want %q", writeErr.Status, "ota_begin_failed")
	}
	if writeErr.HTTPStatus != 500 {
		t.Errorf("HTTPStatus : got %d, want 500", writeErr.HTTPStatus)
	}
}

func TestOTAWrite_RangeMismatch_HTTP416(t *testing.T) {
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		w.Write([]byte(`{"error":"range_mismatch","written":512}`)) //nolint:errcheck
	})

	err := cli.OTAWrite("deploy-1", "sha256:abc", 4, strings.NewReader("data"))
	var writeErr *agent.OTAWriteError
	if !errors.As(err, &writeErr) {
		t.Fatalf("attendu *OTAWriteError, got %T : %v", err, err)
	}
	if writeErr.Status != "range_mismatch" {
		t.Errorf("Status : got %q, want %q", writeErr.Status, "range_mismatch")
	}
	if writeErr.HTTPStatus != 416 {
		t.Errorf("HTTPStatus : got %d, want 416", writeErr.HTTPStatus)
	}
}

func TestGetConfig(t *testing.T) {
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodGet, "/v1alpha1/config")
		json.NewEncoder(w).Encode(agent.ConfigResponse{
			Generation:       3,
			ActiveGeneration: 2,
			NVS:              map[string]string{"gpio_led": "9", "ntp_server": "pool.ntp.org"},
		})
	})

	cfg, err := cli.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig : %v", err)
	}
	if cfg.Generation != 3 {
		t.Errorf("Generation : got %d, want 3", cfg.Generation)
	}
	if cfg.NVS["gpio_led"] != "9" {
		t.Errorf("NVS gpio_led : got %q, want %q", cfg.NVS["gpio_led"], "9")
	}
}

func TestPostConfig_SendsBody(t *testing.T) {
	var body map[string]interface{}
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodPost, "/v1alpha1/config")
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	})

	data := map[string]string{"gpio_btn": "4", "ntp_srv": "ntp.local"}
	if err := cli.PostConfig(data); err != nil {
		t.Fatalf("PostConfig : %v", err)
	}
	d, _ := body["data"].(map[string]interface{})
	if d["gpio_btn"] != "4" {
		t.Errorf("body.data.gpio_btn : got %v, want %q", d["gpio_btn"], "4")
	}
}

func TestPostReboot(t *testing.T) {
	called := false
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodPost, "/v1alpha1/reboot")
		called = true
		w.WriteHeader(http.StatusOK)
	})

	if err := cli.PostReboot(); err != nil {
		t.Fatalf("PostReboot : %v", err)
	}
	if !called {
		t.Error("POST /reboot n'a pas été appelé")
	}
}

func TestRotateToken(t *testing.T) {
	var gotAuth, gotToken string
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodPost, "/v1alpha1/token")
		gotAuth = r.Header.Get("Authorization")
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b) //nolint:errcheck
		gotToken = b["token"]
		w.WriteHeader(http.StatusOK)
	})

	if err := cli.RotateToken("new-secret-xyz"); err != nil {
		t.Fatalf("RotateToken : %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization : got %q, want Bearer test-token", gotAuth)
	}
	if gotToken != "new-secret-xyz" {
		t.Errorf("token dans le body : got %q, want %q", gotToken, "new-secret-xyz")
	}
}

func TestPostAppPort_SendsBody(t *testing.T) {
	var gotPort int
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodPost, "/v1alpha1/app/port")
		var b map[string]int
		json.NewDecoder(r.Body).Decode(&b) //nolint:errcheck
		gotPort = b["port"]
		w.WriteHeader(http.StatusOK)
	})

	if err := cli.PostAppPort(9090); err != nil {
		t.Fatalf("PostAppPort : %v", err)
	}
	if gotPort != 9090 {
		t.Errorf("port dans le body : got %d, want 9090", gotPort)
	}
}

func TestPostAppPort_OutOfRange_RejectedClientSide(t *testing.T) {
	called := false
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	for _, port := range []int{80, 1023, 65536, 100000} {
		if err := cli.PostAppPort(port); err == nil {
			t.Errorf("PostAppPort(%d) : attendu une erreur (hors 1024-65535)", port)
		}
	}
	if called {
		t.Error("aucune requête ne devait partir pour un port hors limites")
	}
}

func TestPostTLSCert_SendsBody(t *testing.T) {
	var gotCert, gotKey string
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodPost, "/v1alpha1/tls/cert")
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b) //nolint:errcheck
		gotCert = b["cert_pem"]
		gotKey = b["key_pem"]
		w.WriteHeader(http.StatusOK)
	})

	if err := cli.PostTLSCert("-----BEGIN CERTIFICATE-----\n...", "-----BEGIN EC PRIVATE KEY-----\n..."); err != nil {
		t.Fatalf("PostTLSCert : %v", err)
	}
	if gotCert != "-----BEGIN CERTIFICATE-----\n..." {
		t.Errorf("cert_pem : got %q", gotCert)
	}
	if gotKey != "-----BEGIN EC PRIVATE KEY-----\n..." {
		t.Errorf("key_pem : got %q", gotKey)
	}
}

func TestOTAPrepare_Accepted(t *testing.T) {
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodPost, "/v1alpha1/ota/prepare")
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req["artifact"] == nil {
			t.Error("champ artifact absent dans PrepareRequest")
		}
		json.NewEncoder(w).Encode(agent.PrepareResponse{Accepted: true, TargetSlot: "ota_1"})
	})

	resp, err := cli.OTAPrepare(agent.PrepareRequest{
		DeploymentID: "fw-1.0.0",
		Artifact:     "registry.local/embewi/fw:v1.0.0",
		Digest:       "sha256:abc",
		Size:         983040,
		Chip:         "esp32s3",
		IDFVersion:   "5.2",
	})
	if err != nil {
		t.Fatalf("OTAPrepare : %v", err)
	}
	if !resp.Accepted {
		t.Errorf("Accepted : got false, want true")
	}
	if resp.TargetSlot != "ota_1" {
		t.Errorf("TargetSlot : got %q, want %q", resp.TargetSlot, "ota_1")
	}
}

// TestOTAWrite_MultiChunk_SendsCorrectRangesAndReconstructsContent couvre l'écart
// d'audit 2026-07-23 : PUT /ota/write doit chunker (Content-Range), pas transférer le
// firmware en un seul PUT monolithique. Firmware de 2.5 chunks → 3 plages attendues.
func TestOTAWrite_MultiChunk_SendsCorrectRangesAndReconstructsContent(t *testing.T) {
	size := int64(otaChunkSize*2 + 100)
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 251)
	}

	var mu sync.Mutex
	var ranges []string
	var received bytes.Buffer

	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertReq(t, r, http.MethodPut, "/v1alpha1/ota/write")
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		ranges = append(ranges, r.Header.Get("Content-Range"))
		received.Write(body)
		isLast := len(ranges) == 3
		mu.Unlock()

		status := "partial"
		if isLast {
			status = "written"
		}
		json.NewEncoder(w).Encode(map[string]string{"status": status}) //nolint:errcheck
	})

	if err := cli.OTAWrite("deploy-1", "sha256:abc", size, bytes.NewReader(content)); err != nil {
		t.Fatalf("OTAWrite : %v", err)
	}

	wantRanges := []string{
		fmt.Sprintf("bytes 0-%d/%d", otaChunkSize-1, size),
		fmt.Sprintf("bytes %d-%d/%d", otaChunkSize, 2*otaChunkSize-1, size),
		fmt.Sprintf("bytes %d-%d/%d", 2*otaChunkSize, size-1, size),
	}
	if len(ranges) != len(wantRanges) {
		t.Fatalf("nombre de plages : got %d %v, want %d %v", len(ranges), ranges, len(wantRanges), wantRanges)
	}
	for i, want := range wantRanges {
		if ranges[i] != want {
			t.Errorf("plage[%d] : got %q, want %q", i, ranges[i], want)
		}
	}
	if !bytes.Equal(received.Bytes(), content) {
		t.Error("contenu reconstruit côté serveur ne correspond pas au firmware d'origine")
	}
}

// TestOTAWrite_ChunkResync416_ResendsOnlyRemainder vérifie que sur un 416 rapportant un
// offset partiel à l'intérieur de la plage envoyée, seule la portion manquante est
// ré-émise — pas toute la plage, et pas tout le firmware.
func TestOTAWrite_ChunkResync416_ResendsOnlyRemainder(t *testing.T) {
	content := []byte("firmware-de-test-court")
	size := int64(len(content))

	var mu sync.Mutex
	attempt := 0
	var secondBody []byte
	var secondRange string

	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		attempt++
		n := attempt
		if n == 2 {
			secondBody = body
			secondRange = r.Header.Get("Content-Range")
		}
		mu.Unlock()

		if n == 1 {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			json.NewEncoder(w).Encode(map[string]int64{"written": 10}) //nolint:errcheck
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "written"}) //nolint:errcheck
	})

	if err := cli.OTAWrite("deploy-1", "sha256:abc", size, bytes.NewReader(content)); err != nil {
		t.Fatalf("OTAWrite : %v", err)
	}

	wantRange := fmt.Sprintf("bytes 10-%d/%d", size-1, size)
	if secondRange != wantRange {
		t.Errorf("Content-Range du retry : got %q, want %q", secondRange, wantRange)
	}
	if string(secondBody) != string(content[10:]) {
		t.Errorf("body du retry : got %q, want %q (seulement le reliquat)", secondBody, content[10:])
	}
}

// TestOTAWrite_ChunkStalls416_GivesUpAfterMaxAttempts vérifie qu'un device qui ne
// progresse jamais (416 avec le même offset en boucle) ne bloque pas indéfiniment.
func TestOTAWrite_ChunkStalls416_GivesUpAfterMaxAttempts(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	cli := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		json.NewEncoder(w).Encode(map[string]int64{"written": 0}) //nolint:errcheck
	})

	err := cli.OTAWrite("deploy-1", "sha256:abc", 4, strings.NewReader("data"))
	if err == nil {
		t.Fatal("attendu une erreur après stagnation répétée, got nil")
	}
	var writeErr *agent.OTAWriteError
	if errors.As(err, &writeErr) {
		t.Errorf("attendu une erreur générique d'abandon, pas *OTAWriteError : %v", err)
	}
	if requests != 3 {
		t.Errorf("nombre de tentatives : got %d, want 3 (otaChunkMaxAttempts)", requests)
	}
}
