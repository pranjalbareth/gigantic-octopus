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
	"regexp"
	"strings"
)

// An Authenticator answers one question: who is making this request?
//
// The answer becomes the Kubernetes username the portal impersonates, so it
// always carries a prefix ("github:", "dev:"). Without the prefix, a person
// who registered the login "kubernetes-admin" would become the cluster admin.
type Authenticator interface {
	// User returns the Kubernetes username for the request, or false.
	User(r *http.Request) (string, bool)
	// DevLogin reports whether the portal serves its own login page.
	DevLogin() bool
}

// ProxyAuth trusts identity headers set by oauth2-proxy.
//
// It is only safe when nothing but the proxy can reach the portal. The
// shipped manifests run the proxy as a sidecar and bind the portal to
// 127.0.0.1, so the only way in is through the login.
type ProxyAuth struct {
	// Headers are tried in order; the first non-empty one wins.
	Headers []string
	// Prefix is put in front of the header value, for example "github:".
	Prefix string
}

// DefaultProxyHeaders are the headers oauth2-proxy sends to its upstream.
var DefaultProxyHeaders = []string{"X-Forwarded-Preferred-Username", "X-Forwarded-User", "X-Forwarded-Email"}

// User implements Authenticator.
func (a ProxyAuth) User(r *http.Request) (string, bool) {
	for _, h := range a.Headers {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return a.Prefix + v, true
		}
	}
	return "", false
}

// DevLogin implements Authenticator.
func (ProxyAuth) DevLogin() bool { return false }

const (
	devCookie = "runway_user"
	devPrefix = "dev:"
)

var devNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,29}$`)

// DevAuth lets anyone sign in as any name, with no password. It exists for
// demos and local clusters, where it shows per-user behaviour without an
// identity provider. Never expose it to a network you do not trust.
type DevAuth struct{}

// User implements Authenticator.
func (DevAuth) User(r *http.Request) (string, bool) {
	c, err := r.Cookie(devCookie)
	if err != nil || !devNamePattern.MatchString(c.Value) {
		return "", false
	}
	return devPrefix + c.Value, true
}

// DevLogin implements Authenticator.
func (DevAuth) DevLogin() bool { return true }

// displayName strips the provider prefix for showing a user their own name.
func displayName(user string) string {
	if _, name, ok := strings.Cut(user, ":"); ok {
		return name
	}
	return user
}
