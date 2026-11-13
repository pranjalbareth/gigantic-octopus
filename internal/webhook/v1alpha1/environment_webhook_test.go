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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

var nameCounter int

func uniqueName(prefix string) string {
	nameCounter++
	return fmt.Sprintf("%s-%d", prefix, nameCounter)
}

// clientAs returns a client that the API server authenticates as the given
// user (through impersonation). The user is an ordinary one: all it may do
// comes from the environment-user ClusterRole that ships in config/rbac.
func clientAs(user string) client.Client {
	GinkgoHelper()
	userCfg := rest.CopyConfig(cfg)
	userCfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	c, err := client.New(userCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return c
}

func newTemplate(name string) *runwayv1alpha1.EnvironmentTemplate {
	return &runwayv1alpha1.EnvironmentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: runwayv1alpha1.EnvironmentTemplateSpec{
			Image:    "nginx:1.27",
			Port:     8080,
			Replicas: 1,
			Quota: runwayv1alpha1.TemplateQuota{
				CPU:    resource.MustParse("1"),
				Memory: resource.MustParse("1Gi"),
				Pods:   4,
			},
			MaxTTL:      metav1.Duration{Duration: 8 * time.Hour},
			MaxLifetime: metav1.Duration{Duration: 24 * time.Hour},
		},
	}
}

func newEnvironment(name, template string, ttl time.Duration) *runwayv1alpha1.Environment {
	return &runwayv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: runwayv1alpha1.EnvironmentSpec{
			Template: template,
			TTL:      metav1.Duration{Duration: ttl},
		},
	}
}

var _ = Describe("Environment admission", func() {
	var (
		tmplName string
		user     string
		asUser   client.Client
	)

	BeforeEach(func() {
		tmplName = uniqueName("web")
		user = uniqueName("user")
		asUser = clientAs(user)
		Expect(k8sClient.Create(ctx, newTemplate(tmplName))).To(Succeed())
	})

	Context("defaulting webhook", func() {
		It("sets the owner to the authenticated user, a 24h TTL and the template label", func() {
			env := &runwayv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: uniqueName("pr")},
				Spec:       runwayv1alpha1.EnvironmentSpec{Template: tmplName},
			}
			// The template only allows 8h at creation, so use one that fits the default.
			big := newTemplate(uniqueName("big"))
			big.Spec.MaxTTL.Duration = 48 * time.Hour
			big.Spec.MaxLifetime.Duration = 48 * time.Hour
			Expect(k8sClient.Create(ctx, big)).To(Succeed())
			env.Spec.Template = big.Name

			Expect(asUser.Create(ctx, env)).To(Succeed())
			Expect(env.Spec.Owner).To(Equal(user))
			Expect(env.Spec.TTL.Duration).To(Equal(24 * time.Hour))
			Expect(env.Labels).To(HaveKeyWithValue(runwayv1alpha1.LabelTemplate, big.Name))
		})
	})

	Context("validating webhook (rules that need other objects)", func() {
		It("refuses an owner other than the caller", func() {
			env := newEnvironment(uniqueName("pr"), tmplName, time.Hour)
			env.Spec.Owner = "someone-else"
			err := asUser.Create(ctx, env)
			Expect(err).To(MatchError(ContainSubstring("must be empty or your own username")))
		})

		It("refuses a template that does not exist", func() {
			err := asUser.Create(ctx, newEnvironment(uniqueName("pr"), "no-such-template", time.Hour))
			Expect(err).To(MatchError(ContainSubstring("spec.template: Not found")))
		})

		It("refuses a TTL above the template's maxTTL at creation", func() {
			err := asUser.Create(ctx, newEnvironment(uniqueName("pr"), tmplName, 9*time.Hour))
			Expect(err).To(MatchError(ContainSubstring("allows at most 8h0m0s at creation")))
		})

		It("allows three environments per owner and refuses the fourth", func() {
			for range MaxEnvironmentsPerOwner {
				Expect(asUser.Create(ctx, newEnvironment(uniqueName("pr"), tmplName, time.Hour))).To(Succeed())
			}
			err := asUser.Create(ctx, newEnvironment(uniqueName("pr"), tmplName, time.Hour))
			Expect(err).To(MatchError(ContainSubstring("already has 3 environments (limit 3)")))

			By("not counting them against a different user")
			Expect(clientAs(uniqueName("other")).Create(ctx, newEnvironment(uniqueName("pr"), tmplName, time.Hour))).To(Succeed())
		})

		It("lets spec.ttl be extended up to maxLifetime and no further", func() {
			env := newEnvironment(uniqueName("pr"), tmplName, 8*time.Hour)
			Expect(asUser.Create(ctx, env)).To(Succeed())

			env.Spec.TTL.Duration = 24 * time.Hour
			Expect(asUser.Update(ctx, env)).To(Succeed())

			env.Spec.TTL.Duration = 25 * time.Hour
			Expect(asUser.Update(ctx, env)).To(MatchError(ContainSubstring("total lifetime of at most 24h0m0s")))
		})

		It("does not block updates that leave the TTL alone, even if the template is gone", func() {
			tmpl := newTemplate(uniqueName("gone"))
			Expect(k8sClient.Create(ctx, tmpl)).To(Succeed())
			env := newEnvironment(uniqueName("pr"), tmpl.Name, time.Hour)
			Expect(asUser.Create(ctx, env)).To(Succeed())

			// Remove the template behind the webhook's back by pretending no
			// Environment uses it: this is what a broken cluster looks like.
			validator := &EnvironmentValidator{Reader: emptyReader{}}
			changed := env.DeepCopy()
			changed.Finalizers = []string{runwayv1alpha1.Finalizer}
			_, err := validator.ValidateUpdate(ctx, env, changed)
			Expect(err).NotTo(HaveOccurred())
		})

		It("never blocks an update on an object that is being deleted", func() {
			validator := &EnvironmentValidator{Reader: emptyReader{}}
			old := newEnvironment("x", "missing", time.Hour)
			deleting := old.DeepCopy()
			now := metav1.Now()
			deleting.DeletionTimestamp = &now
			deleting.Spec.TTL.Duration = 999 * time.Hour
			_, err := validator.ValidateUpdate(ctx, old, deleting)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("admission policy (CEL rules that need only the object)", func() {
		DescribeTable("refuses names that cannot become a namespace",
			func(name string) {
				err := asUser.Create(ctx, newEnvironment(name, tmplName, time.Hour))
				Expect(err).To(MatchError(ContainSubstring("metadata.name must be")))
			},
			Entry("too short", "ab"),
			Entry("contains a dot (fine for the API server, not for a namespace)", "pr.1423"),
			Entry("longer than 40 characters", "a123456789b123456789c123456789d123456789e"),
		)

		It("refuses a TTL under one minute or over 168h", func() {
			err := asUser.Create(ctx, newEnvironment(uniqueName("pr"), tmplName, 30*time.Second))
			Expect(err).To(MatchError(ContainSubstring("spec.ttl must be between 1m and 168h")))

			huge := newTemplate(uniqueName("huge"))
			huge.Spec.MaxTTL.Duration = 1000 * time.Hour
			huge.Spec.MaxLifetime.Duration = 1000 * time.Hour
			Expect(k8sClient.Create(ctx, huge)).To(Succeed())
			err = asUser.Create(ctx, newEnvironment(uniqueName("pr"), huge.Name, 169*time.Hour))
			Expect(err).To(MatchError(ContainSubstring("spec.ttl must be between 1m and 168h")))
		})

		It("makes owner and template immutable", func() {
			env := newEnvironment(uniqueName("pr"), tmplName, time.Hour)
			Expect(asUser.Create(ctx, env)).To(Succeed())

			stolen := env.DeepCopy()
			stolen.Spec.Owner = "mallory"
			Expect(k8sClient.Update(ctx, stolen)).To(MatchError(ContainSubstring("spec.owner cannot be changed")))

			other := newTemplate(uniqueName("other"))
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			moved := env.DeepCopy()
			moved.Spec.Template = other.Name
			Expect(k8sClient.Update(ctx, moved)).To(MatchError(ContainSubstring("spec.template cannot be changed")))
		})
	})

	Context("ownership policy (CEL, asks the authorizer)", func() {
		It("lets only the owner or an administrator change or delete an Environment", func() {
			env := newEnvironment(uniqueName("pr"), tmplName, time.Hour)
			Expect(asUser.Create(ctx, env)).To(Succeed())
			mallory := clientAs(uniqueName("mallory"))

			By("refusing another user's extension")
			theirs := env.DeepCopy()
			theirs.Spec.TTL.Duration = 2 * time.Hour
			err := mallory.Update(ctx, theirs)
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)
			Expect(err).To(MatchError(ContainSubstring("only the owner (" + user + ") can change or delete")))

			By("refusing another user's delete")
			err = mallory.Delete(ctx, env.DeepCopy())
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)

			By("allowing the owner to extend")
			env.Spec.TTL.Duration = 2 * time.Hour
			Expect(asUser.Update(ctx, env)).To(Succeed())

			By("allowing an administrator (the controller holds the same verb) to update")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: env.Name}, env)).To(Succeed())
			env.Finalizers = []string{runwayv1alpha1.Finalizer}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())
			env.Finalizers = nil
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			By("allowing the owner to delete")
			Expect(asUser.Delete(ctx, env)).To(Succeed())
		})

		It("does not let ordinary users touch templates", func() {
			err := asUser.Create(ctx, newTemplate(uniqueName("sneaky")))
			Expect(apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)
		})
	})

	Context("EnvironmentTemplate webhook", func() {
		It("refuses to delete a template that is in use, and allows it afterwards", func() {
			env := newEnvironment(uniqueName("pr"), tmplName, time.Hour)
			Expect(asUser.Create(ctx, env)).To(Succeed())

			tmpl := &runwayv1alpha1.EnvironmentTemplate{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tmplName}, tmpl)).To(Succeed())
			Expect(k8sClient.Delete(ctx, tmpl)).To(MatchError(ContainSubstring("still used by environments: " + env.Name)))

			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			Expect(k8sClient.Delete(ctx, tmpl)).To(Succeed())
		})

		It("refuses inconsistent templates", func() {
			bad := newTemplate(uniqueName("bad"))
			bad.Spec.MaxTTL.Duration = 48 * time.Hour // above maxLifetime (24h)
			bad.Spec.Replicas = 5                     // above quota.pods (4)
			err := k8sClient.Create(ctx, bad)
			Expect(err).To(MatchError(ContainSubstring("must be at least maxTTL")))
			Expect(err).To(MatchError(ContainSubstring("must fit in quota.pods")))
		})
	})
})
