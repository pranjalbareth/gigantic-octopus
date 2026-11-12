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
	"context"
	"fmt"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

const (
	// DefaultTTL is used when an Environment is created without spec.ttl.
	DefaultTTL = 24 * time.Hour
	// MaxEnvironmentsPerOwner is how many Environments one user may have at once.
	MaxEnvironmentsPerOwner = 3
	// maxLabelValueLength is the Kubernetes limit for a label value.
	maxLabelValueLength = 63
)

var environmentKind = runwayv1alpha1.SchemeGroupVersion.WithKind("Environment").GroupKind()

// SetupEnvironmentWebhookWithManager registers the webhook for Environment in the manager.
func SetupEnvironmentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &runwayv1alpha1.Environment{}).
		// Admission reads go straight to the API server instead of the
		// informer cache, so an Environment created a moment ago is counted.
		WithValidator(&EnvironmentValidator{Reader: mgr.GetAPIReader()}).
		WithDefaulter(&EnvironmentDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-runway-pranjalbareth-github-io-v1alpha1-environment,mutating=true,failurePolicy=fail,sideEffects=None,groups=runway.pranjalbareth.github.io,resources=environments,verbs=create;update,versions=v1alpha1,name=menvironment-v1alpha1.kb.io,admissionReviewVersions=v1

// EnvironmentDefaulter fills in the fields a user may leave out.
type EnvironmentDefaulter struct{}

// Default implements admission.Defaulter so a webhook will be registered for the Kind Environment.
func (d *EnvironmentDefaulter) Default(ctx context.Context, obj *runwayv1alpha1.Environment) error {
	if obj.Spec.TTL.Duration == 0 {
		obj.Spec.TTL.Duration = DefaultTTL
	}

	if len(obj.Spec.Template) <= maxLabelValueLength {
		if obj.Labels == nil {
			obj.Labels = map[string]string{}
		}
		obj.Labels[runwayv1alpha1.LabelTemplate] = obj.Spec.Template
	}

	// On create, the owner is whoever the API server authenticated. Users do
	// not have to type it, and the validator rejects any other value, so the
	// per-owner limit cannot be dodged by naming someone else.
	if req, err := admission.RequestFromContext(ctx); err == nil &&
		req.Operation == admissionv1.Create && obj.Spec.Owner == "" {
		obj.Spec.Owner = req.UserInfo.Username
	}
	return nil
}

// The verbs stay create;update on purpose: with failurePolicy=fail, also
// intercepting DELETE would let a webhook outage block cleanup.
// +kubebuilder:webhook:path=/validate-runway-pranjalbareth-github-io-v1alpha1-environment,mutating=false,failurePolicy=fail,sideEffects=None,groups=runway.pranjalbareth.github.io,resources=environments,verbs=create;update,versions=v1alpha1,name=venvironment-v1alpha1.kb.io,admissionReviewVersions=v1

// EnvironmentValidator enforces the rules that need other objects: the
// template must exist, the TTL must fit the template, and each owner gets a
// limited number of Environments. Rules that only need the object itself live
// in the ValidatingAdmissionPolicy under config/admission.
type EnvironmentValidator struct {
	Reader client.Reader
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type Environment.
func (v *EnvironmentValidator) ValidateCreate(ctx context.Context, obj *runwayv1alpha1.Environment) (admission.Warnings, error) {
	spec := field.NewPath("spec")

	if req, err := admission.RequestFromContext(ctx); err == nil && obj.Spec.Owner != req.UserInfo.Username {
		return nil, invalid(obj, field.Forbidden(spec.Child("owner"),
			fmt.Sprintf("must be empty or your own username %q", req.UserInfo.Username)))
	}

	tmpl, ferr := v.template(ctx, obj)
	if ferr != nil {
		return nil, invalid(obj, ferr)
	}
	if obj.Spec.TTL.Duration > tmpl.Spec.MaxTTL.Duration {
		return nil, invalid(obj, field.Invalid(spec.Child("ttl"), obj.Spec.TTL.Duration.String(),
			fmt.Sprintf("template %q allows at most %s at creation", tmpl.Name, tmpl.Spec.MaxTTL.Duration)))
	}

	// Environments that are still being deleted count too: their namespace
	// and workloads exist until the finalizer lets go.
	var all runwayv1alpha1.EnvironmentList
	if err := v.Reader.List(ctx, &all); err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	owned := 0
	for i := range all.Items {
		if all.Items[i].Spec.Owner == obj.Spec.Owner {
			owned++
		}
	}
	if owned >= MaxEnvironmentsPerOwner {
		return nil, invalid(obj, field.Forbidden(spec.Child("owner"),
			fmt.Sprintf("%q already has %d environments (limit %d); delete one first", obj.Spec.Owner, owned, MaxEnvironmentsPerOwner)))
	}
	return nil, nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type Environment.
func (v *EnvironmentValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *runwayv1alpha1.Environment) (admission.Warnings, error) {
	// Never stand in the way of deletion: removing the finalizer is an update.
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	// Only a TTL change needs checking. Everything else (labels, the
	// controller adding its finalizer) passes without reading the template.
	if newObj.Spec.TTL.Duration == oldObj.Spec.TTL.Duration {
		return nil, nil
	}

	tmpl, ferr := v.template(ctx, newObj)
	if ferr != nil {
		return nil, invalid(newObj, ferr)
	}
	if newObj.Spec.TTL.Duration > tmpl.Spec.MaxLifetime.Duration {
		return nil, invalid(newObj, field.Invalid(field.NewPath("spec", "ttl"), newObj.Spec.TTL.Duration.String(),
			fmt.Sprintf("template %q allows a total lifetime of at most %s", tmpl.Name, tmpl.Spec.MaxLifetime.Duration)))
	}
	return nil, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type Environment.
// It is never called because the webhook is not registered for DELETE.
func (v *EnvironmentValidator) ValidateDelete(context.Context, *runwayv1alpha1.Environment) (admission.Warnings, error) {
	return nil, nil
}

func (v *EnvironmentValidator) template(ctx context.Context, env *runwayv1alpha1.Environment) (*runwayv1alpha1.EnvironmentTemplate, *field.Error) {
	path := field.NewPath("spec", "template")
	tmpl := &runwayv1alpha1.EnvironmentTemplate{}
	err := v.Reader.Get(ctx, types.NamespacedName{Name: env.Spec.Template}, tmpl)
	switch {
	case apierrors.IsNotFound(err):
		return nil, field.NotFound(path, env.Spec.Template)
	case err != nil:
		return nil, field.InternalError(path, err)
	}
	return tmpl, nil
}

func invalid(env *runwayv1alpha1.Environment, err *field.Error) error {
	return apierrors.NewInvalid(environmentKind, env.Name, field.ErrorList{err})
}
