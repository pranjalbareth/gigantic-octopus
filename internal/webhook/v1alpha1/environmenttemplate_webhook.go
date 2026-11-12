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
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

var templateResource = runwayv1alpha1.SchemeGroupVersion.WithResource("environmenttemplates").GroupResource()
var templateKind = runwayv1alpha1.SchemeGroupVersion.WithKind("EnvironmentTemplate").GroupKind()

// SetupEnvironmentTemplateWebhookWithManager registers the webhook for EnvironmentTemplate in the manager.
func SetupEnvironmentTemplateWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &runwayv1alpha1.EnvironmentTemplate{}).
		WithValidator(&EnvironmentTemplateValidator{Reader: mgr.GetAPIReader()}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-runway-pranjalbareth-github-io-v1alpha1-environmenttemplate,mutating=false,failurePolicy=fail,sideEffects=None,groups=runway.pranjalbareth.github.io,resources=environmenttemplates,verbs=create;update;delete,versions=v1alpha1,name=venvironmenttemplate-v1alpha1.kb.io,admissionReviewVersions=v1

// EnvironmentTemplateValidator keeps templates consistent and refuses to
// delete one that Environments still depend on.
type EnvironmentTemplateValidator struct {
	Reader client.Reader
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type EnvironmentTemplate.
func (v *EnvironmentTemplateValidator) ValidateCreate(_ context.Context, obj *runwayv1alpha1.EnvironmentTemplate) (admission.Warnings, error) {
	return nil, validateTemplate(obj)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type EnvironmentTemplate.
func (v *EnvironmentTemplateValidator) ValidateUpdate(_ context.Context, _, newObj *runwayv1alpha1.EnvironmentTemplate) (admission.Warnings, error) {
	return nil, validateTemplate(newObj)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type EnvironmentTemplate.
func (v *EnvironmentTemplateValidator) ValidateDelete(ctx context.Context, obj *runwayv1alpha1.EnvironmentTemplate) (admission.Warnings, error) {
	var all runwayv1alpha1.EnvironmentList
	if err := v.Reader.List(ctx, &all); err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	var users []string
	for i := range all.Items {
		if all.Items[i].Spec.Template == obj.Name {
			users = append(users, all.Items[i].Name)
		}
	}
	if len(users) > 0 {
		slices.Sort(users)
		return nil, apierrors.NewForbidden(templateResource, obj.Name,
			fmt.Errorf("still used by environments: %s", strings.Join(users, ", ")))
	}
	return nil, nil
}

func validateTemplate(tmpl *runwayv1alpha1.EnvironmentTemplate) error {
	spec := field.NewPath("spec")
	var errs field.ErrorList

	if tmpl.Spec.MaxTTL.Duration <= 0 {
		errs = append(errs, field.Invalid(spec.Child("maxTTL"), tmpl.Spec.MaxTTL.Duration.String(), "must be greater than zero"))
	}
	if tmpl.Spec.MaxLifetime.Duration < tmpl.Spec.MaxTTL.Duration {
		errs = append(errs, field.Invalid(spec.Child("maxLifetime"), tmpl.Spec.MaxLifetime.Duration.String(),
			fmt.Sprintf("must be at least maxTTL (%s)", tmpl.Spec.MaxTTL.Duration)))
	}
	if tmpl.Spec.Replicas > tmpl.Spec.Quota.Pods {
		errs = append(errs, field.Invalid(spec.Child("replicas"), tmpl.Spec.Replicas,
			fmt.Sprintf("must fit in quota.pods (%d)", tmpl.Spec.Quota.Pods)))
	}
	if tmpl.Spec.Quota.CPU.Sign() <= 0 {
		errs = append(errs, field.Invalid(spec.Child("quota", "cpu"), tmpl.Spec.Quota.CPU.String(), "must be greater than zero"))
	}
	if tmpl.Spec.Quota.Memory.Sign() <= 0 {
		errs = append(errs, field.Invalid(spec.Child("quota", "memory"), tmpl.Spec.Quota.Memory.String(), "must be greater than zero"))
	}

	if len(errs) > 0 {
		return apierrors.NewInvalid(templateKind, tmpl.Name, errs)
	}
	return nil
}
