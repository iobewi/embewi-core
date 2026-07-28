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

// SecretRef référence un Secret Kubernetes par nom + namespace optionnel.
type SecretRef struct {
	// Name : nom du Secret.
	Name string `json:"name"`
	// Namespace : namespace du Secret. Défaut = namespace du McuNode si absent.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

type McuNodeSpec struct {
	// NodeID doit correspondre exactement au EMBEWI_NODE_ID compilé dans le firmware.
	NodeID string `json:"nodeId"`

	// TokenRef référence le Secret K8s portant le token Bearer de ce device (§1 contrat).
	// Le Secret doit contenir data["token"] = <bearer brut>.
	// Exemple : Secret "embewi-a1b2c3-token" → data["token"] = <bearer>.
	//
	// Rotation sans coupure (§4 contrat) : pour faire tourner le token, écrire en
	// une seule opération data["token"]=newToken ET data["previousToken"]=oldToken.
	// Le Core rejoue POST /token avec previousToken tant que le device n'a pas
	// confirmé newToken par heartbeat ; previousToken est alors effacé
	// automatiquement. Ne pas écrire data["token"] seul pendant une rotation — sans
	// previousToken, un crash Core juste après l'écriture du Secret rend le device
	// injoignable en inbound (seul recours : portail captif).
	TokenRef SecretRef `json:"tokenRef"`

	// TLSSecretRef référence un Secret K8s de type kubernetes.io/tls (clés
	// data["tls.crt"]/data["tls.key"]) à pousser vers le device via POST
	// /tls/cert (§4 contrat) — rotation de certificat sans cycle OTA. Absent =
	// le device garde son certificat auto-signé de build. Compatible avec un
	// Secret géré par cert-manager (renouvellement automatique transparent).
	// +optional
	TLSSecretRef SecretRef `json:"tlsSecretRef,omitempty"`
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
	HeapFree         int     `json:"heapFree,omitempty"`
	RSSI             int     `json:"rssi,omitempty"`
	UptimeMs         int64   `json:"uptimeMs,omitempty"`
	ConfigGeneration int     `json:"configGeneration,omitempty"`
	TempCelsius      float64 `json:"tempCelsius,omitempty"`
	TaskHwmMin       int     `json:"taskHwmMin,omitempty"`

	// Capacités hardware (peuplées depuis GET /info au premier contact).
	Chip       string `json:"chip,omitempty"`
	IDFVersion string `json:"idfVersion,omitempty"`
	FlashSize  int64  `json:"flashSize,omitempty"`
	RAMSize    int64  `json:"ramSize,omitempty"`
	AppPort    int    `json:"appPort,omitempty"`

	// ApiVersion : version de protocole négociée avec le device (contrat §4,
	// découverte de version d'API). Vide tant qu'aucun GET /info n'a réussi.
	ApiVersion string `json:"apiVersion,omitempty"`

	// TLSCertDigest : sha256(tls.crt+tls.key) du dernier certificat TLS appliqué
	// avec succès (POST /tls/cert confirmé + reboot). Pas d'équivalent
	// generation/active_generation côté agent pour le cert (contrairement à la
	// config §4a) : ce suivi est purement Core-side, sert à détecter qu'un
	// nouveau push est nécessaire quand Spec.TLSSecretRef change.
	TLSCertDigest string `json:"tlsCertDigest,omitempty"`

	// LastRebootRequested reflète la dernière valeur de l'annotation
	// embewi.io/reboot-requested traitée avec succès (POST /reboot). Pattern
	// kubectl rollout restart : l'annotation sert de nonce opaque (ex. un
	// timestamp) — valeur différente de ce champ → reboot déclenché. Pas de
	// nettoyage d'annotation requis, contrairement à un flag booléen.
	LastRebootRequested string `json:"lastRebootRequested,omitempty"`

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
