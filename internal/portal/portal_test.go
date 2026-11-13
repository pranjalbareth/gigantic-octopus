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
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

var nameCounter int

const (
	nameField     = "name"
	templateField = "template"
	ttlField      = "ttl"
)

func uniqueName(prefix string) string {
	nameCounter++
	return fmt.Sprintf("%s-%d", prefix, nameCounter)
}

func newTemplate(name string) *runwayv1alpha1.EnvironmentTemplate {
	return &runwayv1alpha1.EnvironmentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: runwayv1alpha1.EnvironmentTemplateSpec{
			Image: "nginx:1.27", Port: 80, Replicas: 1,
			Quota:       runwayv1alpha1.TemplateQuota{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), Pods: 5},
			MaxTTL:      metav1.Duration{Duration: 8 * time.Hour},
			MaxLifetime: metav1.Duration{Duration: 12 * time.Hour},
		},
	}
}

// startPortal runs a portal the way cmd/portal does, with the minimal
// identity from the suite, and returns its address.
func startPortal(auth Authenticator) string {
	GinkgoHelper()
	clients, err := NewClients(portalCfg, scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())
	handler, err := (&Server{Clients: clients, Auth: auth, Log: logr.Discard()}).Handler()
	Expect(err).NotTo(HaveOccurred())
	srv := httptest.NewServer(handler)
	DeferCleanup(srv.Close)
	return srv.URL
}

// browser is one person's session against a dev-login portal.
type browser struct {
	base string
	name string
}

func (b browser) do(method, path string, form url.Values, htmx bool) (int, string) {
	GinkgoHelper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, b.base+path, body)
	Expect(err).NotTo(HaveOccurred())
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if b.name != "" {
		req.AddCookie(&http.Cookie{Name: devCookie, Value: b.name})
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return resp.StatusCode, string(out)
}

func (b browser) page() string {
	GinkgoHelper()
	code, body := b.do(http.MethodGet, "/", nil, false)
	Expect(code).To(Equal(http.StatusOK))
	return body
}

func (b browser) start(name, template, ttl string) string {
	GinkgoHelper()
	code, body := b.do(http.MethodPost, "/environments", url.Values{nameField: {name}, templateField: {template}, ttlField: {ttl}}, true)
	Expect(code).To(Equal(http.StatusOK))
	return body
}

func getEnv(name string) *runwayv1alpha1.Environment {
	GinkgoHelper()
	env := &runwayv1alpha1.Environment{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, env)).To(Succeed())
	return env
}

var _ = Describe("Portal", func() {
	var (
		base  string
		tmpl  string
		alice browser
		bob   browser
	)

	BeforeEach(func() {
		base = startPortal(DevAuth{})
		tmpl = uniqueName("web")
		Expect(k8sClient.Create(ctx, newTemplate(tmpl))).To(Succeed())
		alice = browser{base: base, name: uniqueName("alice")}
		bob = browser{base: base, name: uniqueName("bob")}
	})

	It("sends a visitor who is not signed in to the login page", func() {
		code, _ := browser{base: base}.do(http.MethodGet, "/", nil, false)
		Expect(code).To(Equal(http.StatusSeeOther))

		code, body := browser{base: base}.do(http.MethodGet, "/login", nil, false)
		Expect(code).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("Who are you?"))
	})

	It("signs in through the login form and rejects names that cannot be users", func() {
		form := url.Values{nameField: {"Carol"}}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).
			PostForm(base+"/login", form)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusSeeOther))
		Expect(resp.Header.Get("Location")).To(Equal("/"))
		Expect(resp.Cookies()).To(HaveLen(1))
		Expect(resp.Cookies()[0].Value).To(Equal("carol"))

		code, _ := browser{base: base}.do(http.MethodPost, "/login", url.Values{nameField: {"system:admin"}}, false)
		Expect(code).To(Equal(http.StatusSeeOther))
		code, _ = browser{base: base, name: "system:admin"}.do(http.MethodGet, "/environments", nil, true)
		Expect(code).To(Equal(http.StatusUnauthorized))
	})

	It("starts an environment owned by the signed-in user, through the real webhook", func() {
		name := uniqueName("pr")
		body := alice.start(name, tmpl, "2h")
		Expect(body).To(ContainSubstring("Started " + name + "."))
		Expect(body).To(ContainSubstring(`data-name="` + name + `"`))

		env := getEnv(name)
		Expect(env.Spec.Owner).To(Equal("dev:"+alice.name), "the webhook must see the impersonated user, not the portal")
		Expect(env.Spec.TTL.Duration).To(Equal(2 * time.Hour))

		By("showing the phase and the port-forward command once the controller has provisioned it")
		Eventually(alice.page, 10*time.Second, 200*time.Millisecond).Should(And(
			ContainSubstring("Provisioning"),
			ContainSubstring("kubectl port-forward -n env-"+name+" service/"+name+" 8080:80"),
			ContainSubstring("1 of 3 in use"),
		))
	})

	It("shows each user only their own environments", func() {
		hers, his := uniqueName("hers"), uniqueName("his")
		alice.start(hers, tmpl, "1h")
		bob.start(his, tmpl, "1h")

		Expect(alice.page()).To(And(ContainSubstring(hers), Not(ContainSubstring(his))))
		Expect(bob.page()).To(And(ContainSubstring(his), Not(ContainSubstring(hers))))
	})

	It("lets only the owner extend or delete, and shows the cluster's reason otherwise", func() {
		name := uniqueName("pr")
		alice.start(name, tmpl, "1h")

		By("bob trying to extend alice's environment")
		_, body := bob.do(http.MethodPost, "/environments/"+name+"/extend", url.Values{"by": {"1h"}}, true)
		Expect(body).To(ContainSubstring("Only the owner (dev:" + alice.name + ") can change or delete this environment"))
		Expect(getEnv(name).Spec.TTL.Duration).To(Equal(time.Hour))

		By("bob trying to delete it")
		_, body = bob.do(http.MethodDelete, "/environments/"+name, nil, true)
		Expect(body).To(ContainSubstring("Only the owner"))
		Expect(getEnv(name).DeletionTimestamp.IsZero()).To(BeTrue())

		By("alice extending it")
		_, body = alice.do(http.MethodPost, "/environments/"+name+"/extend", url.Values{"by": {"4h"}}, true)
		Expect(body).To(ContainSubstring("Extended " + name + " by 4h."))
		Expect(getEnv(name).Spec.TTL.Duration).To(Equal(5 * time.Hour))

		By("alice extending past the template's 12h lifetime")
		_, body = alice.do(http.MethodPost, "/environments/"+name+"/extend", url.Values{"by": {"8h"}}, true)
		Expect(body).To(ContainSubstring("allows a total lifetime of at most 12h0m0s"))
		Expect(getEnv(name).Spec.TTL.Duration).To(Equal(5 * time.Hour))

		By("alice deleting it")
		_, body = alice.do(http.MethodDelete, "/environments/"+name, nil, true)
		Expect(body).To(ContainSubstring("Deleting " + name + "."))
		Eventually(func() bool { return !getEnv(name).DeletionTimestamp.IsZero() }).Should(BeTrue())
		Expect(alice.page()).To(ContainSubstring(`class="env ending"`))
	})

	It("refuses a fourth environment with the webhook's own message", func() {
		for range MaxEnvironments {
			Expect(alice.start(uniqueName("pr"), tmpl, "1h")).To(ContainSubstring("Started "))
		}
		body := alice.start(uniqueName("pr"), tmpl, "1h")
		Expect(body).To(ContainSubstring("already has 3 environments (limit 3); delete one first"))
		Expect(body).To(ContainSubstring("3 of 3 in use"))
	})

	DescribeTable("turns refusals into one plain sentence",
		func(name, template, ttl, want string) {
			if template == "" {
				template = tmpl
			}
			Expect(alice.start(name, template, ttl)).To(ContainSubstring(`class="notice error" role="alert">` + want))
		},
		Entry("bad lifetime text", "pr-good", "", "soon", "Lifetime must look like 30m, 8h or 1h30m"),
		Entry("lifetime above the template's maximum", "pr-good", "", "9h", "Template "),
		Entry("lifetime under a minute (CEL policy)", "pr-good", "", "30s", "Spec.ttl must be between 1m and 168h"),
		Entry("a name with a dot (CEL policy)", "pr.1", "", "1h", "Metadata.name must be 3-40 lowercase letters"),
		Entry("a template that does not exist", "pr-good", "nope", "1h", "Spec.template"),
	)

	It("refuses state-changing requests that do not come from the page (CSRF)", func() {
		name := uniqueName("pr")
		code, _ := alice.do(http.MethodPost, "/environments", url.Values{nameField: {name}, templateField: {tmpl}, ttlField: {"1h"}}, false)
		Expect(code).To(Equal(http.StatusForbidden))
		code, _ = alice.do(http.MethodDelete, "/environments/"+name, nil, false)
		Expect(code).To(Equal(http.StatusForbidden))
	})

	It("keeps no state of its own: a new portal process shows the same environments", func() {
		name := uniqueName("pr")
		alice.start(name, tmpl, "1h")
		restarted := browser{base: startPortal(DevAuth{}), name: alice.name}
		Expect(restarted.page()).To(ContainSubstring(name))
	})

	It("pushes an event to the browser when an environment changes", func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
		Expect(err).NotTo(HaveOccurred())
		req.AddCookie(&http.Cookie{Name: devCookie, Value: alice.name})
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.Header.Get("Content-Type")).To(Equal("text/event-stream"))

		lines := make(chan string, 100)
		go func() {
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
		}()
		Eventually(lines, 5*time.Second).Should(Receive(Equal("data: resync")), "the stream opens with one catch-up event")

		name := uniqueName("pr")
		bob.start(name, tmpl, "1h")
		Eventually(lines, 5*time.Second).Should(Receive(Equal("data: ADDED")))
	})

	Context("behind oauth2-proxy", func() {
		It("takes the user from the proxy's header, prefixed, and refuses requests without one", func() {
			proxied := startPortal(ProxyAuth{Headers: DefaultProxyHeaders, Prefix: "github:"})

			code, _ := browser{base: proxied}.do(http.MethodGet, "/", nil, false)
			Expect(code).To(Equal(http.StatusUnauthorized), "no header means no access, and no dev login page either")
			code, _ = browser{base: proxied}.do(http.MethodGet, "/login", nil, false)
			Expect(code).To(Equal(http.StatusNotFound))

			name := uniqueName("pr")
			req, err := http.NewRequest(http.MethodPost, proxied+"/environments",
				strings.NewReader(url.Values{nameField: {name}, templateField: {tmpl}, ttlField: {"1h"}}.Encode()))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("HX-Request", "true")
			// A GitHub login that happens to match a cluster user's name.
			req.Header.Set("X-Forwarded-User", "kubernetes-admin")
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusOK))

			Expect(getEnv(name).Spec.Owner).To(Equal("github:kubernetes-admin"),
				"the prefix keeps a login name from ever being a cluster user")
		})
	})

	Context("kubectl plugin", func() {
		It("lists, extends and describes as an ordinary user of the same cluster", func() {
			By("building the plugin and giving it a kubeconfig for a real user")
			dir := GinkgoT().TempDir()
			bin := filepath.Join(dir, "kubectl-runway")
			out, err := exec.Command("go", "build", "-o", bin, "../../cmd/kubectl-runway").CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(out))

			user := uniqueName("dana")
			authed, err := testEnv.AddUser(envtest.User{Name: user}, nil)
			Expect(err).NotTo(HaveOccurred())
			kubeconfig, err := authed.KubeConfig()
			Expect(err).NotTo(HaveOccurred())
			kubeconfigPath := filepath.Join(dir, "kubeconfig")
			Expect(os.WriteFile(kubeconfigPath, kubeconfig, 0o600)).To(Succeed())
			plugin := func(args ...string) (string, error) {
				cmd := exec.Command(bin, args...)
				cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
				out, err := cmd.CombinedOutput()
				return string(out), err
			}

			name := uniqueName("pr")
			dana := impersonate(user)
			Expect(dana.Create(ctx, &runwayv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec:       runwayv1alpha1.EnvironmentSpec{Template: tmpl, TTL: metav1.Duration{Duration: time.Hour}},
			})).To(Succeed())
			alice.start(uniqueName("not-danas"), tmpl, "1h")
			Eventually(func() *metav1.Time { return getEnv(name).Status.ExpiresAt }, 10*time.Second, 100*time.Millisecond).ShouldNot(BeNil())

			By("list showing only the caller's environments")
			listed, err := plugin("list")
			Expect(err).NotTo(HaveOccurred(), listed)
			Expect(listed).To(And(ContainSubstring(name), Not(ContainSubstring("not-danas"))))
			everyone, err := plugin("list", "--all")
			Expect(err).NotTo(HaveOccurred(), everyone)
			Expect(everyone).To(And(ContainSubstring("not-danas"), ContainSubstring("OWNER")))

			By("extend raising spec.ttl")
			extended, err := plugin("extend", name, "2h")
			Expect(err).NotTo(HaveOccurred(), extended)
			Expect(getEnv(name).Spec.TTL.Duration).To(Equal(3 * time.Hour))

			By("extend past the template's lifetime failing with the webhook's reason")
			refused, err := plugin("extend", name, "20h")
			Expect(err).To(HaveOccurred())
			Expect(refused).To(ContainSubstring("total lifetime of at most 12h0m0s"))

			described, err := plugin("describe", name)
			Expect(err).NotTo(HaveOccurred(), described)
			Expect(described).To(And(ContainSubstring("Owner:      "+user), ContainSubstring("Namespace:  env-"+name)))
		})
	})

	// Not a test: set RUNWAY_PORTAL_HOLD=127.0.0.1:18080 to keep a seeded
	// portal running for a look in a browser.
	It("serves a seeded portal for manual inspection", func() {
		addr := os.Getenv("RUNWAY_PORTAL_HOLD")
		if addr == "" {
			Skip("set RUNWAY_PORTAL_HOLD to use")
		}
		seedAndHold(addr, tmpl)
	})
})
