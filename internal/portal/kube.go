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

package portal

import (
	"net/http"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// Clients hands out one Kubernetes client per signed-in user. Each client
// impersonates that user, so the API server applies the user's own RBAC, the
// admission policies and the webhooks to every call. The portal's own
// service account can do one thing: impersonate.
//
// This is why the portal needs no authorization code and no database. It
// cannot do anything a user could not do with kubectl, and everything it
// shows is read from the cluster at the moment of the request.
type Clients struct {
	cfg    *rest.Config
	scheme *runtime.Scheme
	mapper meta.RESTMapper
	http   *http.Client

	mu     sync.Mutex
	byUser map[string]client.WithWatch
}

// NewClients prepares the shared parts: one HTTP transport and one REST
// mapper, so signing in does not trigger API discovery again.
func NewClients(cfg *rest.Config, scheme *runtime.Scheme) (*Clients, error) {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	return &Clients{cfg: cfg, scheme: scheme, mapper: mapper, http: httpClient, byUser: map[string]client.WithWatch{}}, nil
}

// For returns the client that acts as the given user.
func (c *Clients) For(user string) (client.WithWatch, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.byUser[user]; ok {
		return cl, nil
	}
	cfg := rest.CopyConfig(c.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	cl, err := client.NewWithWatch(cfg, client.Options{Scheme: c.scheme, Mapper: c.mapper})
	if err != nil {
		return nil, err
	}
	c.byUser[user] = cl
	return cl, nil
}

// friendly turns an API error into the sentence a person needs. Admission
// errors arrive wrapped in several layers of "who denied what"; the useful
// part is the rule's own message at the end.
func friendly(err error) string {
	msg := err.Error()
	if status, ok := err.(apierrors.APIStatus); ok && status.Status().Message != "" {
		msg = status.Status().Message
	}
	for _, marker := range []string{"denied the request: ", "denied request: "} {
		if _, after, ok := strings.Cut(msg, marker); ok {
			msg = after
		}
	}
	// `Environment.runway… "x" is invalid: spec.owner: Forbidden: reason`
	if _, after, ok := strings.Cut(msg, " is invalid: "); ok {
		msg = after
		for _, kind := range []string{": Forbidden: ", ": Invalid value: ", ": Not found: "} {
			if field, reason, ok := strings.Cut(msg, kind); ok {
				switch kind {
				case ": Not found: ":
					msg = field + " " + reason + " does not exist"
				case ": Invalid value: ":
					// `"100h0m0s": template … allows at most …`
					if _, why, ok := strings.Cut(reason, ": "); ok {
						reason = why
					}
					msg = reason
				default:
					msg = reason
				}
				break
			}
		}
	}
	if msg == "" {
		return "The cluster refused the request."
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}
