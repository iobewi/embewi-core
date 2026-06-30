package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/embewi/core/internal/metrics"
)

func lbl(nodeID string) prometheus.Labels {
	return prometheus.Labels{"node_id": nodeID, "workload": "wl", "chip": "esp32s3"}
}

// base envoie un heartbeat minimal valide pour initialiser les gauges du node.
func base(nodeID string) metrics.HeartbeatData {
	return metrics.HeartbeatData{NodeID: nodeID, Workload: "wl", Chip: "esp32s3"}
}

func TestUpdateFromHeartbeat_TempFilter(t *testing.T) {
	id := "node-temp-filter"

	// Valeur valide : la gauge doit être écrite.
	metrics.UpdateFromHeartbeat(metrics.HeartbeatData{NodeID: id, Workload: "wl", Chip: "esp32s3", TempCelsius: 38.0})
	if got := testutil.ToFloat64(metrics.TemperatureCelsius.With(lbl(id))); got != 38.0 {
		t.Errorf("après temp valide : got %.1f, want 38.0", got)
	}

	// Sentinelle -127.0 : la gauge ne doit pas changer.
	metrics.UpdateFromHeartbeat(metrics.HeartbeatData{NodeID: id, Workload: "wl", Chip: "esp32s3", TempCelsius: -127.0})
	if got := testutil.ToFloat64(metrics.TemperatureCelsius.With(lbl(id))); got != 38.0 {
		t.Errorf("après sentinelle -127.0 : got %.1f, want 38.0 (doit rester inchangé)", got)
	}
}

func TestUpdateFromHeartbeat_TSFilter(t *testing.T) {
	id := "node-ts-filter"

	// TS valide.
	metrics.UpdateFromHeartbeat(metrics.HeartbeatData{NodeID: id, Workload: "wl", Chip: "esp32s3", TS: 1_700_000_000})
	if got := testutil.ToFloat64(metrics.LastHeartbeatTimestamp.With(lbl(id))); got != 1_700_000_000 {
		t.Errorf("après TS valide : got %.0f, want 1700000000", got)
	}

	// TS=0 (NTP pas encore sync) : la gauge ne doit pas être écrasée.
	metrics.UpdateFromHeartbeat(metrics.HeartbeatData{NodeID: id, Workload: "wl", Chip: "esp32s3", TS: 0})
	if got := testutil.ToFloat64(metrics.LastHeartbeatTimestamp.With(lbl(id))); got != 1_700_000_000 {
		t.Errorf("après TS=0 : got %.0f, want 1700000000 (doit rester inchangé)", got)
	}
}

func TestUpdateFromHeartbeat_OtaValidated(t *testing.T) {
	id := "node-ota"
	l := lbl(id)

	metrics.UpdateFromHeartbeat(metrics.HeartbeatData{NodeID: id, Workload: "wl", Chip: "esp32s3", OtaValidated: true})
	if got := testutil.ToFloat64(metrics.OtaValidated.With(l)); got != 1.0 {
		t.Errorf("ota_validated=true : got %.0f, want 1", got)
	}

	metrics.UpdateFromHeartbeat(metrics.HeartbeatData{NodeID: id, Workload: "wl", Chip: "esp32s3", OtaValidated: false})
	if got := testutil.ToFloat64(metrics.OtaValidated.With(l)); got != 0.0 {
		t.Errorf("ota_validated=false : got %.0f, want 0", got)
	}
}

func TestUpdateFromHeartbeat_UptimeConversion(t *testing.T) {
	id := "node-uptime"
	metrics.UpdateFromHeartbeat(metrics.HeartbeatData{NodeID: id, Workload: "wl", Chip: "esp32s3", UptimeMs: 120_034})
	// 120034 ms → 120.034 s
	if got := testutil.ToFloat64(metrics.UptimeSeconds.With(lbl(id))); got != 120.034 {
		t.Errorf("uptime_ms=120034 → uptime_seconds : got %v, want 120.034", got)
	}
}
