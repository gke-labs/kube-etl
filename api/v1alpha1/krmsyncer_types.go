package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceRule defines criteria for what to sync.
// +kubebuilder:object:generate=true
type ResourceRule struct {
	// Group is the API group of the resource
	Group string `json:"group"`
	// Version is the API version of the resource
	Version string `json:"version"`
	// Kind is the Kind of the resource
	Kind string `json:"kind"`

	// Namespaces to filter source objects.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
}

// DestinationConfig defines where to push resources.
// +kubebuilder:object:generate=true
type DestinationConfig struct {
	// KubeConfigSecretRef is the reference to the secret containing the
	// kubeconfig of the destination cluster.
	// +optional
	KubeConfigSecretRef *corev1.SecretReference `json:"kubeConfigSecretRef,omitempty"`
}

// KRMSyncerSpec defines the desired state.
// +kubebuilder:object:generate=true
type KRMSyncerSpec struct {
	// Suspend tells the controller to suspend the sync operations.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Destination defines the target for the sync.
	Destination DestinationConfig `json:"destination"`

	// Rules defines which resources to watch and sync.
	Rules []ResourceRule `json:"rules"`
}

// KRMSyncerStatus defines the observed state.
// +kubebuilder:object:generate=true
type KRMSyncerStatus struct {
	// Conditions of the Syncer (e.g., DestinationUnreachable, Suspended).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// KRMSyncer is the Schema for the krmsyncers API
type KRMSyncer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KRMSyncerSpec   `json:"spec,omitempty"`
	Status KRMSyncerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KRMSyncerList contains a list of KRMSyncer
type KRMSyncerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KRMSyncer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KRMSyncer{}, &KRMSyncerList{})
}
