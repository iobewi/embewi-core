package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// McuConfigMap porte la configuration runtime poussée vers le NVS d'un device ESP.
// Découple la config matérielle (GPIOs, adresses I²C…) du binaire OTA.
// Référencé depuis un McuDeployment via spec.configMapRef (§7a contrat).
//
// Limites NVS agent (§4a) à respecter avant push :
//   - Clé  : 15 caractères max
//   - Valeur : 63 caractères max
//
// +kubebuilder:object:root=true
type McuConfigMap struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Data : paires clé/valeur poussées vers le NVS via POST /config.
	// Sémantique merge-on-key : seules les clés citées sont écrites, les autres inchangées.
	Data map[string]string `json:"data,omitempty"`
}

// +kubebuilder:object:root=true
type McuConfigMapList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []McuConfigMap `json:"items"`
}

func init() {
	SchemeBuilder.Register(&McuConfigMap{}, &McuConfigMapList{})
}
