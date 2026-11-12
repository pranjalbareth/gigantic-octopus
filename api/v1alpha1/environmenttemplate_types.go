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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TemplateQuota is the total an environment built from the template may use.
type TemplateQuota struct {
	// cpu is the total CPU for the namespace, for example "1".
	// +required
	CPU resource.Quantity `json:"cpu"`

	// memory is the total memory for the namespace, for example "1Gi".
	// +required
	Memory resource.Quantity `json:"memory"`

	// pods is the largest number of pods in the namespace.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=50
	Pods int32 `json:"pods"`
}

// EnvironmentTemplateSpec defines the desired state of EnvironmentTemplate
type EnvironmentTemplateSpec struct {
	// image is the default container image.
	// +required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// port is the container port the Service exposes.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// replicas is the number of pods. Defaults to 1.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	Replicas int32 `json:"replicas,omitempty"`

	// quota caps what one environment may use.
	// +required
	Quota TemplateQuota `json:"quota"`

	// maxTTL is the longest TTL allowed when an environment is created.
	// +required
	MaxTTL metav1.Duration `json:"maxTTL"`

	// maxLifetime is the longest total TTL an environment can be extended to.
	// +required
	MaxLifetime metav1.Duration `json:"maxLifetime"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=envtmpl
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="MaxTTL",type=string,JSONPath=`.spec.maxTTL`
// +kubebuilder:printcolumn:name="MaxLifetime",type=string,JSONPath=`.spec.maxLifetime`

// EnvironmentTemplate is the Schema for the environmenttemplates API
type EnvironmentTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of EnvironmentTemplate
	// +required
	Spec EnvironmentTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// EnvironmentTemplateList contains a list of EnvironmentTemplate
type EnvironmentTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []EnvironmentTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &EnvironmentTemplate{}, &EnvironmentTemplateList{})
		return nil
	})
}
