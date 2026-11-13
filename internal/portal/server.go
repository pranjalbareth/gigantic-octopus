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

// Package portal is Runway's self-service web page. It is a thin layer over
// the Kubernetes API: every page is a List, every button is a Create, Update
// or Delete of an Environment, made as the signed-in user.
package portal

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	// MaxEnvironments mirrors the webhook's per-owner limit. It is only
	// used to show "2 of 3"; the webhook is what enforces it.
	MaxEnvironments = 3
	// keepAlive is how often an idle event stream sends a comment line, so
	// proxies do not close it.
	keepAlive = 25 * time.Second
	// rewatchPause is how long to wait before watching again after the API
	// server closes a watch.
	rewatchPause = time.Second
)

// Server serves the portal.
type Server struct {
	Clients *Clients
	Auth    Authenticator
	Log     logr.Logger
	// Now is the clock used for countdowns. Defaults to time.Now.
	Now func() time.Time

	tmpl *template.Template
}

// Handler builds the HTTP routes.
func (s *Server) Handler() (http.Handler, error) {
	if s.Now == nil {
		s.Now = time.Now
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = tmpl

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	if s.Auth.DevLogin() {
		mux.HandleFunc("GET /login", s.loginPage)
		mux.HandleFunc("POST /login", s.login)
		mux.HandleFunc("POST /logout", s.logout)
	}

	mux.HandleFunc("GET /{$}", s.signedIn(s.index))
	mux.HandleFunc("GET /environments", s.signedIn(s.list))
	mux.HandleFunc("GET /events", s.signedIn(s.events))
	mux.HandleFunc("POST /environments", s.signedIn(sameSite(s.create)))
	mux.HandleFunc("POST /environments/{name}/extend", s.signedIn(sameSite(s.extend)))
	mux.HandleFunc("DELETE /environments/{name}", s.signedIn(sameSite(s.remove)))
	return mux, nil
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

type userHandler func(w http.ResponseWriter, r *http.Request, user string, kube client.WithWatch)

// signedIn resolves the user and their impersonating client, or sends the
// request to the login page.
func (s *Server) signedIn(next userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.Auth.User(r)
		if !ok {
			if s.Auth.DevLogin() && r.Method == http.MethodGet && r.Header.Get("HX-Request") == "" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "You are not signed in.", http.StatusUnauthorized)
			return
		}
		kube, err := s.Clients.For(user)
		if err != nil {
			s.Log.Error(err, "Building a client", "user", user)
			http.Error(w, "The portal could not reach the cluster.", http.StatusBadGateway)
			return
		}
		next(w, r, user, kube)
	}
}

// sameSite refuses state-changing requests that were not sent by this page's
// own scripts. Browsers will not let another site add a custom header to a
// form post, so requiring one blocks cross-site request forgery.
func sameSite(next userHandler) userHandler {
	return func(w http.ResponseWriter, r *http.Request, user string, kube client.WithWatch) {
		if r.Header.Get("HX-Request") != "true" {
			http.Error(w, "This request did not come from the portal page.", http.StatusForbidden)
			return
		}
		next(w, r, user, kube)
	}
}

// --- view models -----------------------------------------------------------

type envView struct {
	Name      string
	Template  string
	Phase     string
	Reason    string
	Created   time.Time
	Expires   time.Time
	HasExpiry bool
	Ending    bool
	Forward   string
}

type templateView struct {
	Name        string
	Image       string
	CPU         string
	Memory      string
	Pods        int32
	MaxTTL      string
	MaxLifetime string
}

type pageData struct {
	User      string
	DevLogin  bool
	Envs      []envView
	Templates []templateView
	Limit     int
	Notice    string
	Error     string
	Now       time.Time
}

// short prints a duration the way people write it: "8h", "1h30m", "45s".
func short(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (s *Server) load(r *http.Request, user string, kube client.Client) (pageData, error) {
	data := pageData{User: displayName(user), DevLogin: s.Auth.DevLogin(), Limit: MaxEnvironments, Now: s.Now()}

	var envs runwayv1alpha1.EnvironmentList
	if err := kube.List(r.Context(), &envs); err != nil {
		return data, err
	}
	var tmpls runwayv1alpha1.EnvironmentTemplateList
	if err := kube.List(r.Context(), &tmpls); err != nil {
		return data, err
	}

	ports := map[string]int32{}
	for i := range tmpls.Items {
		t := &tmpls.Items[i]
		ports[t.Name] = t.Spec.Port
		data.Templates = append(data.Templates, templateView{
			Name:        t.Name,
			Image:       t.Spec.Image,
			CPU:         t.Spec.Quota.CPU.String(),
			Memory:      t.Spec.Quota.Memory.String(),
			Pods:        t.Spec.Quota.Pods,
			MaxTTL:      short(t.Spec.MaxTTL.Duration),
			MaxLifetime: short(t.Spec.MaxLifetime.Duration),
		})
	}
	slices.SortFunc(data.Templates, func(a, b templateView) int { return strings.Compare(a.Name, b.Name) })

	for i := range envs.Items {
		e := &envs.Items[i]
		// Everyone may list every Environment (RBAC cannot filter a list by
		// a field), so "mine" is decided here. Writes are different: the
		// ownership policy in the API server enforces those.
		if e.Spec.Owner != user {
			continue
		}
		v := envView{
			Name:     e.Name,
			Template: e.Spec.Template,
			Phase:    string(e.Status.Phase),
			Created:  e.CreationTimestamp.Time,
			Ending:   !e.DeletionTimestamp.IsZero() || e.Status.Phase == runwayv1alpha1.PhaseExpiring,
		}
		if v.Phase == "" {
			v.Phase = string(runwayv1alpha1.PhasePending)
		}
		if e.Status.ExpiresAt != nil {
			v.Expires, v.HasExpiry = e.Status.ExpiresAt.Time, true
		}
		for _, c := range e.Status.Conditions {
			if c.Type == runwayv1alpha1.ConditionReady && c.Status != metav1.ConditionTrue {
				v.Reason = c.Message
			}
		}
		if port, ok := ports[e.Spec.Template]; ok && e.Status.Namespace != "" {
			v.Forward = fmt.Sprintf("kubectl port-forward -n %s service/%s 8080:%d", e.Status.Namespace, e.Name, port)
		}
		data.Envs = append(data.Envs, v)
	}
	// Newest first; names break ties so the order never jumps on refresh.
	slices.SortFunc(data.Envs, func(a, b envView) int {
		if !a.Created.Equal(b.Created) {
			return b.Created.Compare(a.Created)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return data, nil
}

// --- pages -----------------------------------------------------------------

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.Log.Error(err, "Rendering", "template", name)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request, user string, kube client.WithWatch) {
	data, err := s.load(r, user, kube)
	if err != nil {
		data.Error = friendly(err)
	}
	s.render(w, "index.html", data)
}

// list returns only the environments panel. The page asks for it again
// whenever the event stream says something changed.
func (s *Server) list(w http.ResponseWriter, r *http.Request, user string, kube client.WithWatch) {
	s.respond(w, r, user, kube, "", nil)
}

// respond re-renders the environments panel after an action, with either a
// confirmation or the cluster's reason for refusing.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, user string, kube client.WithWatch, notice string, actionErr error) {
	data, err := s.load(r, user, kube)
	if err != nil {
		data.Error = friendly(err)
	}
	if actionErr != nil {
		data.Error = friendly(actionErr)
	} else {
		data.Notice = notice
	}
	s.render(w, "panel", data)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, user string, kube client.WithWatch) {
	name := strings.TrimSpace(r.FormValue("name"))
	ttl, err := time.ParseDuration(strings.TrimSpace(r.FormValue("ttl")))
	if err != nil {
		s.respond(w, r, user, kube, "", fmt.Errorf("lifetime must look like 30m, 8h or 1h30m"))
		return
	}
	env := &runwayv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: runwayv1alpha1.EnvironmentSpec{
			Template: r.FormValue("template"),
			TTL:      metav1.Duration{Duration: ttl},
			Parameters: runwayv1alpha1.EnvironmentParameters{
				Image: strings.TrimSpace(r.FormValue("image")),
			},
		},
	}
	// No owner is set here. The webhook fills it in from the impersonated
	// user, exactly as it does for kubectl.
	if err := kube.Create(r.Context(), env); err != nil {
		if apierrors.IsAlreadyExists(err) {
			err = fmt.Errorf("the name %q is taken; pick another", name)
		}
		s.respond(w, r, user, kube, "", err)
		return
	}
	s.respond(w, r, user, kube, "Started "+name+".", nil)
}

func (s *Server) extend(w http.ResponseWriter, r *http.Request, user string, kube client.WithWatch) {
	name := r.PathValue("name")
	by, err := time.ParseDuration(r.FormValue("by"))
	if err != nil || by <= 0 {
		s.respond(w, r, user, kube, "", fmt.Errorf("extension must look like 1h or 30m"))
		return
	}
	env := &runwayv1alpha1.Environment{}
	if err := kube.Get(r.Context(), types.NamespacedName{Name: name}, env); err != nil {
		s.respond(w, r, user, kube, "", err)
		return
	}
	// Extending is a change to desired state: raise spec.ttl and let the
	// controller move expiresAt. A merge patch sends only that one field.
	patch := client.MergeFrom(env.DeepCopy())
	env.Spec.TTL.Duration += by
	if err := kube.Patch(r.Context(), env, patch); err != nil {
		s.respond(w, r, user, kube, "", err)
		return
	}
	s.respond(w, r, user, kube, fmt.Sprintf("Extended %s by %s.", name, short(by)), nil)
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, user string, kube client.WithWatch) {
	name := r.PathValue("name")
	env := &runwayv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := kube.Delete(r.Context(), env); err != nil && !apierrors.IsNotFound(err) {
		s.respond(w, r, user, kube, "", err)
		return
	}
	s.respond(w, r, user, kube, "Deleting "+name+".", nil)
}

// events streams one Server-Sent Event per change to any Environment. The
// page answers each event by fetching the panel again, so the browser never
// holds state of its own. If the API server ends the watch (it does so
// routinely, and with 410 Gone when the resource version is too old), a new
// watch starts and one event is sent to cover anything missed in between.
func (s *Server) events(w http.ResponseWriter, r *http.Request, _ string, kube client.WithWatch) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming is not supported.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()
	send := func(line string) {
		_, _ = fmt.Fprint(w, line)
		flusher.Flush()
	}
	send(": connected\n\n")

	tick := time.NewTicker(keepAlive)
	defer tick.Stop()

	for {
		w, err := kube.Watch(ctx, &runwayv1alpha1.EnvironmentList{})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.Log.V(1).Info("Watch failed; retrying", "error", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(rewatchPause):
				continue
			}
		}
		send("event: change\ndata: resync\n\n")

		open := true
		for open {
			select {
			case <-ctx.Done():
				w.Stop()
				return
			case <-tick.C:
				send(": keepalive\n\n")
			case ev, ok := <-w.ResultChan():
				if !ok || ev.Type == watch.Error {
					open = false
					break
				}
				send("event: change\ndata: " + string(ev.Type) + "\n\n")
			}
		}
		w.Stop()
	}
}

// --- dev login -------------------------------------------------------------

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]string{"Error": r.URL.Query().Get("error")})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	if !devNamePattern.MatchString(name) {
		http.Redirect(w, r, "/login?error=name", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: devCookie, Value: name, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: devCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
