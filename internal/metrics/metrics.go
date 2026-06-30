// Package metrics expose les gauges Prometheus mcunode_* (§8b contrat).
// Les gauges sont enregistrées dans le registry controller-runtime (même endpoint /metrics).
// Mise à jour à chaque heartbeat reçu via UpdateFromHeartbeat.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	HeapFreeBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcunode_heap_free_bytes",
		Help: "Mémoire heap disponible sur le device (bytes).",
	}, []string{"node_id", "workload", "chip"})

	WifiRssiDbm = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcunode_wifi_rssi_dbm",
		Help: "Force du signal Wi-Fi (dBm).",
	}, []string{"node_id", "workload", "chip"})

	UptimeSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcunode_uptime_seconds",
		Help: "Uptime du device depuis le dernier reboot (secondes).",
	}, []string{"node_id", "workload", "chip"})

	TemperatureCelsius = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcunode_temperature_celsius",
		Help: "Température SoC (°C). Absent si capteur indisponible (sentinelle -127.0 filtrée).",
	}, []string{"node_id", "workload", "chip"})

	TaskStackHwmBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcunode_task_stack_hwm_bytes",
		Help: "High-water mark de la stack tâche principale (bytes).",
	}, []string{"node_id", "workload", "chip"})

	LastHeartbeatTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcunode_last_heartbeat_timestamp",
		Help: "Timestamp Unix (secondes) du dernier heartbeat reçu.",
	}, []string{"node_id", "workload", "chip"})

	ConfigGeneration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcunode_config_generation",
		Help: "Génération de configuration NVS rapportée par le device.",
	}, []string{"node_id", "workload", "chip"})

	OtaValidated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcunode_ota_validated",
		Help: "1 si le firmware courant est validé par le bootloader, 0 sinon.",
	}, []string{"node_id", "workload", "chip"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		HeapFreeBytes,
		WifiRssiDbm,
		UptimeSeconds,
		TemperatureCelsius,
		TaskStackHwmBytes,
		LastHeartbeatTimestamp,
		ConfigGeneration,
		OtaValidated,
	)
}

// HeartbeatData porte les champs d'un heartbeat nécessaires à la mise à jour des gauges.
type HeartbeatData struct {
	NodeID           string
	Workload         string  // deployment_id du heartbeat
	Chip             string  // depuis McuNode.Status.Chip (peut être "" avant le premier GET /info)
	HeapFree         int
	RSSI             int
	UptimeMs         int64
	TempCelsius      float64
	TaskHwmMin       int
	ConfigGeneration int
	TS               int64
	OtaValidated     bool
}

// UpdateFromHeartbeat met à jour toutes les gauges mcunode_* (§8b contrat).
// Appelé à chaque heartbeat reçu depuis le serveur heartbeat.
func UpdateFromHeartbeat(d HeartbeatData) {
	labels := prometheus.Labels{
		"node_id":  d.NodeID,
		"workload": d.Workload,
		"chip":     d.Chip,
	}

	HeapFreeBytes.With(labels).Set(float64(d.HeapFree))
	WifiRssiDbm.With(labels).Set(float64(d.RSSI))
	UptimeSeconds.With(labels).Set(float64(d.UptimeMs) / 1000.0)

	// Sentinelle -127.0 : capteur SoC indisponible, ne pas écrire la gauge.
	if d.TempCelsius != -127.0 {
		TemperatureCelsius.With(labels).Set(d.TempCelsius)
	}

	TaskStackHwmBytes.With(labels).Set(float64(d.TaskHwmMin))

	// ts ≈ 0 si NTP pas encore synchronisé (boot récent) — ne pas écraser une valeur valide.
	if d.TS != 0 {
		LastHeartbeatTimestamp.With(labels).Set(float64(d.TS))
	}

	ConfigGeneration.With(labels).Set(float64(d.ConfigGeneration))

	otaVal := 0.0
	if d.OtaValidated {
		otaVal = 1.0
	}
	OtaValidated.With(labels).Set(otaVal)
}
