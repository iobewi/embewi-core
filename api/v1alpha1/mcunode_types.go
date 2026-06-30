package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// McuNode représente un device ESP physique dans le cluster.
// Le status est entièrement piloté par les heartbeats entrants — jamais édité à la main.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.firmwareVersion`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.ip`,priority=1
type McuNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   McuNodeSpec   `json:"spec,omitempty"`
	Status McuNodeStatus `json:"status,omitempty"`
}

type McuNodeSpec struct {
	// NodeID doit correspondre exactement au EMBEWI_NODE_ID compilé dans le firmware.
	NodeID string `json:"nodeId"`
}

type McuNodeStatus struct {
	// IP de management du device (extraite des heartbeats).
	IP string `json:"ip,omitempty"`

	// État courant de l'agent (booting|pending_verify|running|degraded|rollback|failed|offline).
	// +kubebuilder:validation:Enum=booting;pending_verify;running;degraded;rollback;failed;offline
	State string `json:"state,omitempty"`

	// Informations firmware courant.
	FirmwareName    string `json:"firmwareName,omitempty"`
	FirmwareVersion string `json:"firmwareVersion,omitempty"`
	FirmwareDigest  string `json:"firmwareDigest,omitempty"`

	// DeploymentID du déploiement actuellement validé.
	DeploymentID string `json:"deploymentId,omitempty"`

	// OtaValidated : true uniquement après mark_valid sur le device.
	OtaValidated bool `json:"otaValidated"`

	// Métriques temps réel.
	HeapFree        int     `json:"heapFree,omitempty"`
	RSSI            int     `json:"rssi,omitempty"`
	UptimeMs        int64   `json:"uptimeMs,omitempty"`
	ConfigGeneration int    `json:"configGeneration,omitempty"`
	TempCelsius     float64 `json:"tempCelsius,omitempty"`
	TaskHwmMin      int     `json:"taskHwmMin,omitempty"`

	// Capacités hardware (peuplées depuis GET /info au premier contact).
	Chip       string `json:"chip,omitempty"`
	IDFVersion string `json:"idfVersion,omitempty"`
	FlashSize  int64  `json:"flashSize,omitempty"`
	RAMSize    int64  `json:"ramSize,omitempty"`
	AppPort    int    `json:"appPort,omitempty"`

	// Ready pilote EndpointSlice.ready (§8 contrat).
	// true ssi state==running && ota_validated==true && heartbeat récent.
	Ready bool `json:"ready"`

	// Dernier heartbeat reçu.
	LastHeartbeat *metav1.Time `json:"lastHeartbeat,omitempty"`

	// Conditions standard Kubernetes.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type McuNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []McuNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&McuNode{}, &McuNodeList{})
}
