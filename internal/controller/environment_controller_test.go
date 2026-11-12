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
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	testingclock "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

var nameCounter int

// uniqueName gives every spec its own objects so specs cannot affect each other.
func uniqueName(prefix string) string {
	nameCounter++
	return fmt.Sprintf("%s-%d", prefix, nameCounter)
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
			MaxTTL:      metav1.Duration{Duration: 72 * time.Hour},
			MaxLifetime: metav1.Duration{Duration: 168 * time.Hour},
		},
	}
}

func newEnvironment(name, template string, ttl time.Duration) *runwayv1alpha1.Environment {
	return &runwayv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: runwayv1alpha1.EnvironmentSpec{
			Template: template,
			Owner:    "alice",
			TTL:      metav1.Duration{Duration: ttl},
		},
	}
}

// harness is one "controller process": a reconciler with its own fake clock.
// Building a second harness for the same objects models a restart.
type harness struct {
	reconciler *EnvironmentReconciler
	clock      *testingclock.FakeClock
	recorder   *events.FakeRecorder
}

func newHarness(now time.Time) *harness {
	clk := testingclock.NewFakeClock(now)
	rec := events.NewFakeRecorder(100)
	return &harness{
		clock:    clk,
		recorder: rec,
		reconciler: &EnvironmentReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Clock:    clk,
			Recorder: rec,
		},
	}
}

func (h *harness) reconcile(name string) reconcile.Result {
	GinkgoHelper()
	res, err := h.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	Expect(err).NotTo(HaveOccurred())
	return res
}

// provision runs the two reconciles a new Environment needs: the first takes
// the finalizer, the second creates everything.
func (h *harness) provision(name string) {
	GinkgoHelper()
	h.reconcile(name)
	h.reconcile(name)
}

func (h *harness) drainEvents() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func getEnv(name string) *runwayv1alpha1.Environment {
	GinkgoHelper()
	env := &runwayv1alpha1.Environment{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, env)).To(Succeed())
	return env
}

func readyCondition(env *runwayv1alpha1.Environment) *metav1.Condition {
	return meta.FindStatusCondition(env.Status.Conditions, runwayv1alpha1.ConditionReady)
}

// finishNamespaceDeletion plays the part of the namespace controller, which
// envtest does not run: it clears the namespace's finalizers so the API
// server can remove a namespace that has been deleted.
func finishNamespaceDeletion(name string) {
	GinkgoHelper()
	ns := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, ns); err != nil {
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		return
	}
	Expect(ns.DeletionTimestamp.IsZero()).To(BeFalse(), "namespace %s was expected to be terminating", name)
	ns.Spec.Finalizers = nil
	Expect(k8sClient.SubResource("finalize").Update(ctx, ns)).To(Succeed())
	Eventually(func() bool {
		return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &corev1.Namespace{}))
	}).Should(BeTrue())
}

func markDeploymentAvailable(namespace, name string) {
	GinkgoHelper()
	d := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, d)).To(Succeed())
	d.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentAvailable,
		Status: corev1.ConditionTrue,
		Reason: "MinimumReplicasAvailable",
	}}
	Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
}

var _ = Describe("Environment reconciler", func() {
	var (
		tmplName string
		envName  string
	)

	BeforeEach(func() {
		tmplName = uniqueName("web")
		envName = uniqueName("pr")
		Expect(k8sClient.Create(ctx, newTemplate(tmplName))).To(Succeed())
	})

	Context("provisioning", func() {
		It("creates an isolated namespace with everything in it and reports Ready", func() {
			env := newEnvironment(envName, tmplName, time.Hour)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			h := newHarness(env.CreationTimestamp.Time)

			By("taking the finalizer first and nothing else")
			h.reconcile(envName)
			env = getEnv(envName)
			Expect(env.Finalizers).To(ContainElement(runwayv1alpha1.Finalizer))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: env.NamespaceName()}, &corev1.Namespace{}))).To(BeTrue())

			By("creating the namespace and its contents on the next reconcile")
			res := h.reconcile(envName)
			Expect(res.RequeueAfter).To(Equal(time.Hour), "should ask to be woken exactly at expiry")

			nsName := "env-" + envName
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nsName}, ns)).To(Succeed())
			env = getEnv(envName)
			Expect(metav1.IsControlledBy(ns, env)).To(BeTrue())

			quota := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: objectName}, quota)).To(Succeed())
			Expect(quota.Spec.Hard.Pods().Value()).To(Equal(int64(4)))
			Expect(quota.Spec.Hard[corev1.ResourceLimitsMemory]).To(Equal(resource.MustParse("1Gi")))

			limits := &corev1.LimitRange{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: objectName}, limits)).To(Succeed())
			Expect(limits.Spec.Limits[0].Default.Cpu().MilliValue()).To(Equal(int64(250)), "1 CPU split over 4 pods")

			policy := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: objectName}, policy)).To(Succeed())
			Expect(policy.Spec.Ingress).To(HaveLen(1))
			Expect(policy.Spec.Ingress[0].From[0].NamespaceSelector).To(BeNil(), "only same-namespace pods may connect")

			binding := &rbacv1.RoleBinding{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: "runway-owner"}, binding)).To(Succeed())
			Expect(binding.RoleRef.Name).To(Equal("edit"))
			Expect(binding.Subjects).To(ConsistOf(rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: "User", Name: "alice"}))

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: envName}, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.27"))
			Expect(metav1.IsControlledBy(deploy, env)).To(BeTrue())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: nsName, Name: envName}, svc)).To(Succeed())
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))

			By("reporting Provisioning until the workload is available")
			Expect(env.Status.Phase).To(Equal(runwayv1alpha1.PhaseProvisioning))
			Expect(env.Status.Namespace).To(Equal(nsName))
			Expect(env.Status.URL).To(Equal(fmt.Sprintf("http://%s.%s.svc.cluster.local:8080", envName, nsName)))
			Expect(env.Status.ExpiresAt.Time).To(BeTemporally("==", env.CreationTimestamp.Add(time.Hour)))
			Expect(env.Status.ObservedGeneration).To(Equal(env.Generation))
			Expect(readyCondition(env).Reason).To(Equal(runwayv1alpha1.ReasonProvisioning))

			By("reporting Ready once the Deployment is available")
			markDeploymentAvailable(nsName, envName)
			h.reconcile(envName)
			env = getEnv(envName)
			Expect(env.Status.Phase).To(Equal(runwayv1alpha1.PhaseReady))
			Expect(readyCondition(env).Status).To(Equal(metav1.ConditionTrue))
		})

		It("is idempotent: reconciling again changes nothing", func() {
			env := newEnvironment(envName, tmplName, time.Hour)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			h := newHarness(env.CreationTimestamp.Time)
			h.provision(envName)

			before := getEnv(envName)
			deployBefore := &appsv1.Deployment{}
			key := types.NamespacedName{Namespace: before.NamespaceName(), Name: envName}
			Expect(k8sClient.Get(ctx, key, deployBefore)).To(Succeed())

			h.reconcile(envName)
			h.reconcile(envName)

			after := getEnv(envName)
			deployAfter := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, key, deployAfter)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(deployAfter.ResourceVersion).To(Equal(deployBefore.ResourceVersion))
		})

		It("uses the environment's own image when one is given", func() {
			env := newEnvironment(envName, tmplName, time.Hour)
			env.Spec.Parameters.Image = "ghcr.io/acme/api:pr-1423"
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			newHarness(env.CreationTimestamp.Time).provision(envName)

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "env-" + envName, Name: envName}, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/acme/api:pr-1423"))
		})

		It("rolls out a template change on the next reconcile", func() {
			env := newEnvironment(envName, tmplName, time.Hour)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			h := newHarness(env.CreationTimestamp.Time)
			h.provision(envName)

			tmpl := &runwayv1alpha1.EnvironmentTemplate{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tmplName}, tmpl)).To(Succeed())
			tmpl.Spec.Image = "nginx:1.28"
			tmpl.Spec.Quota.Pods = 8
			Expect(k8sClient.Update(ctx, tmpl)).To(Succeed())
			h.reconcile(envName)

			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "env-" + envName, Name: envName}, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.28"))
			quota := &corev1.ResourceQuota{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "env-" + envName, Name: objectName}, quota)).To(Succeed())
			Expect(quota.Spec.Hard.Pods().Value()).To(Equal(int64(8)))
		})
	})

	Context("expiry", func() {
		It("does nothing one second early and deletes exactly at expiresAt", func() {
			env := newEnvironment(envName, tmplName, 10*time.Minute)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			h := newHarness(env.CreationTimestamp.Time)
			h.provision(envName)

			h.clock.SetTime(env.CreationTimestamp.Add(10*time.Minute - time.Second))
			res := h.reconcile(envName)
			Expect(res.RequeueAfter).To(Equal(time.Second))
			Expect(getEnv(envName).DeletionTimestamp.IsZero()).To(BeTrue())

			h.clock.SetTime(env.CreationTimestamp.Add(10 * time.Minute))
			h.reconcile(envName)
			env = getEnv(envName)
			Expect(env.DeletionTimestamp.IsZero()).To(BeFalse())
			Expect(env.Status.Phase).To(Equal(runwayv1alpha1.PhaseExpiring))
			Expect(readyCondition(env).Reason).To(Equal(runwayv1alpha1.ReasonExpired))
			Expect(h.drainEvents()).To(ContainElement(ContainSubstring("Expired")))
		})

		It("deletes an environment that expired while the controller was down", func() {
			env := newEnvironment(envName, tmplName, 2*time.Minute)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			created := env.CreationTimestamp.Time

			By("running a controller that provisions the environment, then dies")
			newHarness(created).provision(envName)
			Expect(getEnv(envName).Status.Phase).To(Equal(runwayv1alpha1.PhaseProvisioning))

			By("starting a brand-new controller an hour later, with no memory of the first")
			restarted := newHarness(created.Add(time.Hour))

			By("its first reconcile deleting the expired environment")
			restarted.reconcile(envName)
			env = getEnv(envName)
			Expect(env.DeletionTimestamp.IsZero()).To(BeFalse())
			Expect(env.Status.Phase).To(Equal(runwayv1alpha1.PhaseExpiring))

			By("the finalizer deleting the namespace and waiting for it")
			res := restarted.reconcile(envName)
			Expect(res.RequeueAfter).To(Equal(namespaceGonePoll))
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "env-" + envName}, ns)).To(Succeed())
			Expect(ns.DeletionTimestamp.IsZero()).To(BeFalse())
			env = getEnv(envName)
			Expect(env.Status.Phase).To(Equal(runwayv1alpha1.PhaseDeleting))
			Expect(env.Finalizers).To(ContainElement(runwayv1alpha1.Finalizer), "must hold on until the namespace is gone")

			By("releasing the Environment once the namespace is really gone")
			finishNamespaceDeletion("env-" + envName)
			restarted.reconcile(envName)
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: envName}, &runwayv1alpha1.Environment{}))).To(BeTrue())
		})

		It("moves expiry when spec.ttl is raised, and records an event", func() {
			env := newEnvironment(envName, tmplName, time.Hour)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			h := newHarness(env.CreationTimestamp.Time)
			h.provision(envName)
			h.drainEvents()

			h.clock.SetTime(env.CreationTimestamp.Add(59 * time.Minute))
			env = getEnv(envName)
			env.Spec.TTL.Duration = 2 * time.Hour
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			res := h.reconcile(envName)
			Expect(res.RequeueAfter).To(Equal(61 * time.Minute))
			env = getEnv(envName)
			Expect(env.Status.ExpiresAt.Time).To(BeTemporally("==", env.CreationTimestamp.Add(2*time.Hour)))
			Expect(env.DeletionTimestamp.IsZero()).To(BeTrue())
			Expect(h.drainEvents()).To(ContainElement(ContainSubstring("TTLChanged")))
		})
	})

	Context("when provisioning is blocked", func() {
		It("waits for a missing template, then continues when it appears", func() {
			missing := uniqueName("missing")
			env := newEnvironment(envName, missing, time.Hour)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			h := newHarness(env.CreationTimestamp.Time)
			h.provision(envName)

			env = getEnv(envName)
			Expect(env.Status.Phase).To(Equal(runwayv1alpha1.PhasePending))
			Expect(readyCondition(env).Reason).To(Equal(runwayv1alpha1.ReasonTemplateNotFound))

			Expect(k8sClient.Create(ctx, newTemplate(missing))).To(Succeed())
			h.reconcile(envName)
			Expect(getEnv(envName).Status.Phase).To(Equal(runwayv1alpha1.PhaseProvisioning))
		})

		It("still expires an environment whose template is missing", func() {
			env := newEnvironment(envName, uniqueName("missing"), time.Minute)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			h := newHarness(env.CreationTimestamp.Add(2 * time.Minute))
			h.provision(envName)
			Expect(getEnv(envName).DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("never adopts or deletes a namespace it did not create", func() {
			foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "env-" + envName}}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			env := newEnvironment(envName, tmplName, time.Hour)
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			h := newHarness(env.CreationTimestamp.Time)
			h.provision(envName)

			env = getEnv(envName)
			Expect(env.Status.Phase).To(Equal(runwayv1alpha1.PhasePending))
			Expect(readyCondition(env).Reason).To(Equal(runwayv1alpha1.ReasonNamespaceConflict))
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), foreign)).To(Succeed())
			Expect(foreign.OwnerReferences).To(BeEmpty())

			By("deleting the Environment and finding the foreign namespace untouched")
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			h.reconcile(envName)
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: envName}, &runwayv1alpha1.Environment{}))).To(BeTrue())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), foreign)).To(Succeed())
			Expect(foreign.DeletionTimestamp.IsZero()).To(BeTrue())
		})
	})
})

// These specs run the reconciler the way production does: inside a manager,
// driven by watches and the real clock. They prove the wiring that calling
// Reconcile directly cannot: the requeue timer, the owned-object watches and
// the template watch.
var _ = Describe("Environment controller under a manager", Ordered, func() {
	var (
		mgrCancel context.CancelFunc
		mgrDone   chan struct{}
	)

	BeforeAll(func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:     scheme.Scheme,
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&EnvironmentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, mgrCancel = context.WithCancel(ctx)
		mgrDone = make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(mgrDone)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
	})

	AfterAll(func() {
		mgrCancel()
		Eventually(mgrDone, 30*time.Second).Should(BeClosed())
	})

	It("provisions, follows template edits, and expires on its own", func() {
		tmplName := uniqueName("web")
		envName := uniqueName("live")
		Expect(k8sClient.Create(ctx, newTemplate(tmplName))).To(Succeed())
		Expect(k8sClient.Create(ctx, newEnvironment(envName, tmplName, 8*time.Second))).To(Succeed())

		deployKey := types.NamespacedName{Namespace: "env-" + envName, Name: envName}
		Eventually(func() error {
			return k8sClient.Get(ctx, deployKey, &appsv1.Deployment{})
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())

		By("becoming Ready when the owned Deployment reports available (Owns watch)")
		markDeploymentAvailable(deployKey.Namespace, deployKey.Name)
		Eventually(func() runwayv1alpha1.EnvironmentPhase {
			return getEnv(envName).Status.Phase
		}, 5*time.Second, 100*time.Millisecond).Should(Equal(runwayv1alpha1.PhaseReady))

		By("rolling out a template edit without anyone touching the Environment (template watch)")
		tmpl := &runwayv1alpha1.EnvironmentTemplate{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tmplName}, tmpl)).To(Succeed())
		tmpl.Spec.Image = "nginx:1.28"
		Expect(k8sClient.Update(ctx, tmpl)).To(Succeed())
		Eventually(func() string {
			d := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, deployKey, d)).To(Succeed())
			return d.Spec.Template.Spec.Containers[0].Image
		}, 5*time.Second, 100*time.Millisecond).Should(Equal("nginx:1.28"))

		By("deleting itself when the TTL runs out (RequeueAfter)")
		Eventually(func() bool {
			env := &runwayv1alpha1.Environment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: envName}, env); err != nil {
				return apierrors.IsNotFound(err)
			}
			return !env.DeletionTimestamp.IsZero()
		}, 20*time.Second, 200*time.Millisecond).Should(BeTrue())

		By("going away completely once the namespace is gone (Namespace watch)")
		Eventually(func() bool {
			ns := &corev1.Namespace{}
			return k8sClient.Get(ctx, types.NamespacedName{Name: "env-" + envName}, ns) == nil && !ns.DeletionTimestamp.IsZero()
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
		finishNamespaceDeletion("env-" + envName)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: envName}, &runwayv1alpha1.Environment{}))
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
	})
})
