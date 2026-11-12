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

package controller

import (
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

// This file only describes what each object should look like. Every function
// fills in an object in place and is safe to run on every reconcile.

const (
	objectName      = "runway"
	containerName   = "app"
	ownerEditRole   = "edit"
	managedByLabel  = "app.kubernetes.io/managed-by"
	managedByRunway = "runway"
)

func labelsFor(env *runwayv1alpha1.Environment) map[string]string {
	return map[string]string{
		managedByLabel:                  managedByRunway,
		runwayv1alpha1.LabelEnvironment: env.Name,
	}
}

func addLabels(obj metav1.Object, env *runwayv1alpha1.Environment) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, labelsFor(env))
	obj.SetLabels(labels)
}

// imageFor picks the environment's own image, or the template's default.
func imageFor(env *runwayv1alpha1.Environment, tmpl *runwayv1alpha1.EnvironmentTemplate) string {
	if env.Spec.Parameters.Image != "" {
		return env.Spec.Parameters.Image
	}
	return tmpl.Spec.Image
}

// urlFor is the in-cluster address of the environment's Service.
func urlFor(env *runwayv1alpha1.Environment, tmpl *runwayv1alpha1.EnvironmentTemplate) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", env.Name, env.NamespaceName(), tmpl.Spec.Port)
}

func mutateQuota(q *corev1.ResourceQuota, tmpl *runwayv1alpha1.EnvironmentTemplate) {
	quota := tmpl.Spec.Quota
	q.Spec.Hard = corev1.ResourceList{
		corev1.ResourceRequestsCPU:    quota.CPU,
		corev1.ResourceRequestsMemory: quota.Memory,
		corev1.ResourceLimitsCPU:      quota.CPU,
		corev1.ResourceLimitsMemory:   quota.Memory,
		corev1.ResourcePods:           *resource.NewQuantity(int64(quota.Pods), resource.DecimalSI),
	}
}

// mutateLimitRange gives every container a default request and limit of
// (quota / max pods). Without it, pods that set no resources are rejected by
// the ResourceQuota.
func mutateLimitRange(lr *corev1.LimitRange, tmpl *runwayv1alpha1.EnvironmentTemplate) {
	quota := tmpl.Spec.Quota
	pods := int64(quota.Pods)
	perContainer := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(quota.CPU.MilliValue()/pods, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(quota.Memory.Value()/pods, resource.BinarySI),
	}
	lr.Spec.Limits = []corev1.LimitRangeItem{{
		Type:           corev1.LimitTypeContainer,
		Default:        perContainer,
		DefaultRequest: perContainer,
	}}
}

// mutateNetworkPolicy denies all ingress except from pods in the same namespace.
func mutateNetworkPolicy(np *networkingv1.NetworkPolicy) {
	np.Spec = networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		Ingress: []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
		}},
	}
}

// mutateRoleBinding gives the owner the built-in "edit" role in the namespace.
func mutateRoleBinding(rb *rbacv1.RoleBinding, env *runwayv1alpha1.Environment) {
	rb.RoleRef = rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "ClusterRole",
		Name:     ownerEditRole,
	}
	rb.Subjects = []rbacv1.Subject{{
		APIGroup: rbacv1.GroupName,
		Kind:     rbacv1.UserKind,
		Name:     env.Spec.Owner,
	}}
}

func mutateDeployment(d *appsv1.Deployment, env *runwayv1alpha1.Environment, tmpl *runwayv1alpha1.EnvironmentTemplate) {
	selector := map[string]string{runwayv1alpha1.LabelEnvironment: env.Name}
	replicas := max(tmpl.Spec.Replicas, 1)
	d.Spec.Replicas = &replicas
	// The selector is immutable once created, so it must never depend on
	// anything that can change.
	d.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
	d.Spec.Template.Labels = labelsFor(env)

	// Edit the first container in place so that fields the API server
	// defaulted are kept and the update is a no-op when nothing changed.
	if len(d.Spec.Template.Spec.Containers) == 0 {
		d.Spec.Template.Spec.Containers = []corev1.Container{{}}
	}
	c := &d.Spec.Template.Spec.Containers[0]
	c.Name = containerName
	c.Image = imageFor(env, tmpl)
	if len(c.Ports) == 0 {
		c.Ports = []corev1.ContainerPort{{}}
	}
	c.Ports[0].ContainerPort = tmpl.Spec.Port
	c.Ports[0].Protocol = corev1.ProtocolTCP
}

func mutateService(s *corev1.Service, env *runwayv1alpha1.Environment, tmpl *runwayv1alpha1.EnvironmentTemplate) {
	s.Spec.Selector = map[string]string{runwayv1alpha1.LabelEnvironment: env.Name}
	if len(s.Spec.Ports) == 0 {
		s.Spec.Ports = []corev1.ServicePort{{}}
	}
	p := &s.Spec.Ports[0]
	p.Port = tmpl.Spec.Port
	p.TargetPort = intstr.FromInt32(tmpl.Spec.Port)
	p.Protocol = corev1.ProtocolTCP
}
