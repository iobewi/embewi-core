package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// McuDeployment décrit un déploiement de firmware sur un device ESP.
// Le contrôleur orchestre : pull OCI → verify → prepare → write → activate → confirm.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.firmware.image`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type McuDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   McuDeploymentSpec   `json:"spec,omitempty"`
	Status McuDeploymentStatus `json:"status,omitempty"`
}

type McuDeploymentSpec struct {
	// NodeName : pin explicite sur un McuNode (recommandé — §7 contrat).
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// NodeSelector : sélection par labels (doit résoudre exactement 1 node).
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	Firmware FirmwareSpec `json:"firmware"`

	// ConfigMapRef : nom d'un McuConfigMap dans le même namespace (§7a contrat).
	// Absent = pas de push config, défauts build actifs.
	// +optional
	ConfigMapRef string `json:"configMapRef,omitempty"`
}

type FirmwareSpec struct {
	// Image OCI de l'artefact firmware (ex: registry.local/embewi/wheel-controller:v1.1.0).
	Image string `json:"image"`

	// Name et Version sont extraits de l'image si absents, mais peuvent être
	// déclarés explicitement pour le matching avec EMBEWI_FW_NAME / FW_VERSION.
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
}

// McuDeploymentPhase décrit la phase courante du déploiement.
type McuDeploymentPhase string

const (
	PhaseBinding      McuDeploymentPhase = "Binding"      // résolution du McuNode cible
	PhasePulling      McuDeploymentPhase = "Pulling"      // pull artefact OCI
	PhasePreparing    McuDeploymentPhase = "Preparing"    // POST /ota/prepare
	PhaseWriting      McuDeploymentPhase = "Writing"      // PUT /ota/write
	PhaseActivating   McuDeploymentPhase = "Activating"   // POST /ota/activate + reboot
	PhaseConfirming   McuDeploymentPhase = "Confirming"   // attente heartbeat running
	PhaseDeployed     McuDeploymentPhase = "Deployed"     // ota_validated=true confirmé
	PhaseFailed       McuDeploymentPhase = "Failed"       // erreur terminale
)

type McuDeploymentStatus struct {
	// Phase courante du déploiement.
	Phase McuDeploymentPhase `json:"phase,omitempty"`

	// McuNode résolu (nom de la ressource K8s).
	BoundNode string `json:"boundNode,omitempty"`

	// DeploymentID transmis au device (clé d'idempotence §6).
	DeploymentID string `json:"deploymentId,omitempty"`

	// Message lisible décrivant l'état ou l'erreur.
	Message string `json:"message,omitempty"`

	// Digest SHA-256 du blob firmware (`sha256:<hex>`).
	Digest string `json:"digest,omitempty"`

	// Size : taille en octets du blob firmware (renseigné après Pulling).
	Size int64 `json:"size,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
type McuDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []McuDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&McuDeployment{}, &McuDeploymentList{})
}
