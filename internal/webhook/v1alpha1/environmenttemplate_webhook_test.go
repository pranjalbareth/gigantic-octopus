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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

// emptyReader is a cluster with nothing in it: every Get is NotFound and
// every List is empty. It lets a spec prove that a code path never needs to
// read the template.
type emptyReader struct{}

func (emptyReader) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewNotFound(runwayv1alpha1.SchemeGroupVersion.WithResource("environmenttemplates").GroupResource(), key.Name)
}

func (emptyReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return nil
}
