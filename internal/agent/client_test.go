package agent_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/embewi/core/internal/agent"
)

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
		json.NewEncoder(w).Encode(agent.InfoResponse{NodeID: "esp32-abc", Chip: "esp32s3", AppPort: 8080})
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

	err := cli.OTAWrite("deploy-1", "sha256:abc", 1024, strings.NewReader("data"))
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
