/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EnvironmentPhase is a one-word summary of where an Environment is in its
// lifecycle. It is computed on every reconcile and never used as input.
type EnvironmentPhase string

const (
	// PhasePending means the Environment is accepted but something blocks
	// provisioning (see the Ready condition for the reason).
	PhasePending EnvironmentPhase = "Pending"
	// PhaseProvisioning means the objects exist but the workload is not available yet.
	PhaseProvisioning EnvironmentPhase = "Provisioning"
	// PhaseReady means the workload is available.
	PhaseReady EnvironmentPhase = "Ready"
	// PhaseExpiring means the TTL has run out and the Environment is being deleted.
	PhaseExpiring EnvironmentPhase = "Expiring"
	// PhaseDeleting means the Environment is waiting for its namespace to be gone.
	PhaseDeleting EnvironmentPhase = "Deleting"
)

const (
	// ConditionReady is the single condition type an Environment reports.
	ConditionReady = "Ready"

	// Reasons used on the Ready condition.
	ReasonAvailable         = "WorkloadAvailable"
	ReasonProvisioning      = "Provisioning"
	ReasonTemplateNotFound  = "TemplateNotFound"
	ReasonNamespaceConflict = "NamespaceConflict"
	ReasonExpired           = "Expired"
	ReasonDeleting          = "Deleting"
)

const (
	// Finalizer makes Environment deletion wait until its namespace is fully gone.
	Finalizer = "runway.pranjalbareth.github.io/finalizer"
	// LabelEnvironment is set on every object the operator creates.
	LabelEnvironment = "runway.pranjalbareth.github.io/environment"
	// LabelTemplate records which template an Environment was created from.
	LabelTemplate = "runway.pranjalbareth.github.io/template"
	// NamespacePrefix is put in front of the Environment name to get its namespace.
	NamespacePrefix = "env-"
)

// EnvironmentParameters are the per-environment overrides of the template.
type EnvironmentParameters struct {
	// image overrides the template's container image.
	// +optional
	Image string `json:"image,omitempty"`
}

// EnvironmentSpec defines the desired state of Environment
type EnvironmentSpec struct {
	// template is the name of the EnvironmentTemplate to build from.
	// +required
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`

	// owner is the user who gets edit rights in the environment's namespace.
	// The defaulting webhook sets it to the authenticated user on create.
	// +optional
	Owner string `json:"owner,omitempty"`

	// ttl is how long the environment lives, counted from its creation.
	// To extend an environment, raise this value. Defaults to 24h.
	// +optional
	TTL metav1.Duration `json:"ttl,omitzero"`

	// parameters override values from the template.
	// +optional
	Parameters EnvironmentParameters `json:"parameters,omitzero"`
}

// EnvironmentStatus defines the observed state of Environment.
type EnvironmentStatus struct {
	// phase is a one-word summary of the lifecycle state.
	// +optional
	Phase EnvironmentPhase `json:"phase,omitempty"`

	// expiresAt is creationTimestamp + spec.ttl. The environment is deleted
	// on the first reconcile at or after this time.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// namespace is the namespace created for this environment.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// url is the in-cluster address of the workload.
	// +optional
	URL string `json:"url,omitempty"`

	// observedGeneration is the spec generation the status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions holds the Ready condition.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=env
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.template`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Expires",type=string,JSONPath=`.status.expiresAt`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`,priority=1

// Environment is the Schema for the environments API
type Environment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Environment
	// +required
	Spec EnvironmentSpec `json:"spec"`

	// status defines the observed state of Environment
	// +optional
	Status EnvironmentStatus `json:"status,omitzero"`
}

// NamespaceName returns the name of the namespace this Environment owns.
func (e *Environment) NamespaceName() string {
	return NamespacePrefix + e.Name
}

// +kubebuilder:object:root=true

// EnvironmentList contains a list of Environment
type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Environment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Environment{}, &EnvironmentList{})
		return nil
	})
}
