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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

const (
	// templateIndex is the cache index from template name to Environments.
	templateIndex = "spec.template"
	// namespaceGonePoll is the fallback for noticing that a namespace has
	// finished terminating. The watch on owned Namespaces normally fires first.
	namespaceGonePoll = 2 * time.Second
)

// EnvironmentReconciler reconciles a Environment object
type EnvironmentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clock is the only source of "now". Tests inject a fake one.
	Clock    clock.PassiveClock
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=runway.pranjalbareth.github.io,resources=environments,verbs=get;list;watch;update;patch;delete;administer
// +kubebuilder:rbac:groups=runway.pranjalbareth.github.io,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=runway.pranjalbareth.github.io,resources=environments/finalizers,verbs=update
// +kubebuilder:rbac:groups=runway.pranjalbareth.github.io,resources=environmenttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces;resourcequotas;limitranges;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=edit,verbs=bind
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves one Environment a step closer to its spec.
//
// It is level-triggered: it reads the current state, decides what should be
// true right now, and makes it so. It keeps no memory between calls, so it
// does not matter why it was called or how many calls were missed.
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Read the Environment. Gone means there is nothing left to do.
	env := &runwayv1alpha1.Environment{}
	if err := r.Get(ctx, req.NamespacedName, env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Being deleted: tear down, then release the finalizer.
	if !env.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, env)
	}

	// 3. Take the finalizer before creating anything, so deletion always
	// passes through finalize. The update triggers the next reconcile.
	if !controllerutil.ContainsFinalizer(env, runwayv1alpha1.Finalizer) {
		patch := client.MergeFrom(env.DeepCopy())
		controllerutil.AddFinalizer(env, runwayv1alpha1.Finalizer)
		return ctrl.Result{}, r.Patch(ctx, env, patch)
	}

	// Everything below edits env.Status in memory; one patch at the end
	// writes whatever changed.
	original := env.DeepCopy()
	now := r.Clock.Now()

	// 4. Work out expiry from the object itself.
	expiresAt := metav1.NewTime(ExpiresAt(env))
	if old := env.Status.ExpiresAt; old != nil && !old.Equal(&expiresAt) {
		r.Recorder.Eventf(env, nil, corev1.EventTypeNormal, "TTLChanged", "Reconcile",
			"Expiry moved from %s to %s", old.UTC().Format(time.RFC3339), expiresAt.UTC().Format(time.RFC3339))
	}
	env.Status.ExpiresAt = &expiresAt
	env.Status.ObservedGeneration = env.Generation

	// 5. Out of time: delete the Environment. Step 2 finishes the job.
	if Expired(env, now) {
		return r.expire(ctx, env, original, now)
	}

	// 6. Make the cluster match the spec.
	blocked, err := r.ensureResources(ctx, env)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 7. Report what we see.
	if !blocked && original.Status.Phase != runwayv1alpha1.PhaseReady && env.Status.Phase == runwayv1alpha1.PhaseReady {
		timeToReady.Observe(now.Sub(env.CreationTimestamp.Time).Seconds())
		r.Recorder.Eventf(env, nil, corev1.EventTypeNormal, "Ready", "Reconcile", "Workload is available at %s", env.Status.URL)
	}
	if err := r.patchStatus(ctx, env, original); err != nil {
		return ctrl.Result{}, err
	}

	// 8. Come back when the TTL runs out. This is a wake-up call, not the
	// source of truth: if it is lost, the next reconcile for any reason
	// (including the full resync after a restart) reaches step 5 anyway.
	log.V(1).Info("Reconciled", "phase", env.Status.Phase, "expiresAt", expiresAt)
	return ctrl.Result{RequeueAfter: Remaining(env, now)}, nil
}

// expire marks the Environment as expiring and deletes it.
func (r *EnvironmentReconciler) expire(ctx context.Context, env, original *runwayv1alpha1.Environment, now time.Time) (ctrl.Result, error) {
	env.Status.Phase = runwayv1alpha1.PhaseExpiring
	setReady(env, metav1.ConditionFalse, runwayv1alpha1.ReasonExpired, "TTL has run out")
	if err := r.patchStatus(ctx, env, original); err != nil {
		return ctrl.Result{}, err
	}

	// The UID precondition makes sure we delete the object we looked at and
	// not a new Environment that was created with the same name meanwhile.
	uid := env.UID
	err := r.Delete(ctx, env, client.Preconditions{UID: &uid})
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	lag := now.Sub(ExpiresAt(env))
	expiryLag.Observe(lag.Seconds())
	r.Recorder.Eventf(env, nil, corev1.EventTypeNormal, "Expired", "Delete",
		"TTL of %s ran out; deleting (%s after expiry)", env.Spec.TTL.Duration, lag.Round(time.Second))
	return ctrl.Result{}, nil
}

// finalize deletes the environment's namespace and waits until it is fully
// gone before letting the Environment go. That makes deletion foreground:
// when `kubectl delete environment` returns, nothing is left behind.
func (r *EnvironmentReconciler) finalize(ctx context.Context, env *runwayv1alpha1.Environment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(env, runwayv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}

	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: env.NamespaceName()}, ns)
	switch {
	case apierrors.IsNotFound(err):
		// Gone. Fall through and release the finalizer.
	case err != nil:
		return ctrl.Result{}, err
	case !metav1.IsControlledBy(ns, env):
		// A namespace with our name that we never owned. Leave it alone.
	default:
		if ns.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		original := env.DeepCopy()
		env.Status.Phase = runwayv1alpha1.PhaseDeleting
		setReady(env, metav1.ConditionFalse, runwayv1alpha1.ReasonDeleting, "Waiting for namespace "+ns.Name+" to be removed")
		if err := r.patchStatus(ctx, env, original); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: namespaceGonePoll}, nil
	}

	patch := client.MergeFrom(env.DeepCopy())
	controllerutil.RemoveFinalizer(env, runwayv1alpha1.Finalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, env, patch))
}

// ensureResources creates or updates everything an Environment owns and sets
// the phase, namespace, URL and Ready condition on env.Status.
//
// blocked is true when provisioning cannot continue until something outside
// the controller changes (a missing template or a namespace name clash).
func (r *EnvironmentReconciler) ensureResources(ctx context.Context, env *runwayv1alpha1.Environment) (blocked bool, err error) {
	tmpl := &runwayv1alpha1.EnvironmentTemplate{}
	if err := r.Get(ctx, types.NamespacedName{Name: env.Spec.Template}, tmpl); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		env.Status.Phase = runwayv1alpha1.PhasePending
		setReady(env, metav1.ConditionFalse, runwayv1alpha1.ReasonTemplateNotFound,
			fmt.Sprintf("EnvironmentTemplate %q does not exist", env.Spec.Template))
		return true, nil
	}

	// Namespace. Never adopt one we did not create: it may hold someone
	// else's workloads, and owning it would delete them on expiry.
	nsName := env.NamespaceName()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ns), ns); err == nil && !metav1.IsControlledBy(ns, env) {
		env.Status.Phase = runwayv1alpha1.PhasePending
		setReady(env, metav1.ConditionFalse, runwayv1alpha1.ReasonNamespaceConflict,
			fmt.Sprintf("Namespace %q already exists and is not owned by this Environment", nsName))
		return true, nil
	} else if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err := r.ensure(ctx, env, ns, func() {}); err != nil {
		return false, err
	}
	env.Status.Namespace = nsName

	fixed := metav1.ObjectMeta{Name: objectName, Namespace: nsName}

	quota := &corev1.ResourceQuota{ObjectMeta: fixed}
	if err := r.ensure(ctx, env, quota, func() { mutateQuota(quota, tmpl) }); err != nil {
		return false, err
	}
	limits := &corev1.LimitRange{ObjectMeta: fixed}
	if err := r.ensure(ctx, env, limits, func() { mutateLimitRange(limits, tmpl) }); err != nil {
		return false, err
	}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: fixed}
	if err := r.ensure(ctx, env, policy, func() { mutateNetworkPolicy(policy) }); err != nil {
		return false, err
	}
	if env.Spec.Owner != "" {
		binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "runway-owner", Namespace: nsName}}
		if err := r.ensure(ctx, env, binding, func() { mutateRoleBinding(binding, env) }); err != nil {
			return false, err
		}
	}

	workload := metav1.ObjectMeta{Name: env.Name, Namespace: nsName}
	deploy := &appsv1.Deployment{ObjectMeta: workload}
	if err := r.ensure(ctx, env, deploy, func() { mutateDeployment(deploy, env, tmpl) }); err != nil {
		return false, err
	}
	svc := &corev1.Service{ObjectMeta: workload}
	if err := r.ensure(ctx, env, svc, func() { mutateService(svc, env, tmpl) }); err != nil {
		return false, err
	}
	env.Status.URL = urlFor(env, tmpl)

	if deploymentAvailable(deploy) {
		env.Status.Phase = runwayv1alpha1.PhaseReady
		setReady(env, metav1.ConditionTrue, runwayv1alpha1.ReasonAvailable, "Workload is available")
	} else {
		env.Status.Phase = runwayv1alpha1.PhaseProvisioning
		setReady(env, metav1.ConditionFalse, runwayv1alpha1.ReasonProvisioning, "Waiting for the workload to become available")
	}
	return false, nil
}

// ensure creates obj if it is missing, or updates it if mutate changes it.
// Every object gets the Environment as its controller owner, which does two
// things: garbage collection removes it with the Environment, and a change to
// it wakes up the reconciler.
func (r *EnvironmentReconciler) ensure(ctx context.Context, env *runwayv1alpha1.Environment, obj client.Object, mutate func()) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		mutate()
		addLabels(obj, env)
		return controllerutil.SetControllerReference(env, obj, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensuring %T %s: %w", obj, client.ObjectKeyFromObject(obj), err)
	}
	return nil
}

// patchStatus writes env.Status if it differs from the original's.
func (r *EnvironmentReconciler) patchStatus(ctx context.Context, env, original *runwayv1alpha1.Environment) error {
	if equality.Semantic.DeepEqual(original.Status, env.Status) {
		return nil
	}
	return r.Status().Patch(ctx, env, client.MergeFrom(original))
}

func setReady(env *runwayv1alpha1.Environment, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               runwayv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: env.Generation,
	})
}

func deploymentAvailable(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// environmentsForTemplate maps a changed template to the Environments built
// from it, so an edit to a template rolls out without waiting for expiry.
func (r *EnvironmentReconciler) environmentsForTemplate(ctx context.Context, tmpl client.Object) []reconcile.Request {
	var list runwayv1alpha1.EnvironmentList
	if err := r.List(ctx, &list, client.MatchingFields{templateIndex: tmpl.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "Listing Environments for template", "template", tmpl.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("runway")
	}
	registerMetrics(metrics.Registry, mgr.GetClient())

	err := mgr.GetFieldIndexer().IndexField(context.Background(), &runwayv1alpha1.Environment{}, templateIndex,
		func(obj client.Object) []string {
			return []string{obj.(*runwayv1alpha1.Environment).Spec.Template}
		})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&runwayv1alpha1.Environment{}).
		// Watching everything we own means drift is repaired at once: delete
		// the Service by hand and the next reconcile puts it back.
		Owns(&corev1.Namespace{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ResourceQuota{}).
		Owns(&corev1.LimitRange{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&rbacv1.RoleBinding{}).
		Watches(&runwayv1alpha1.EnvironmentTemplate{}, handler.EnqueueRequestsFromMapFunc(r.environmentsForTemplate)).
		Named("environment").
		Complete(r)
}
