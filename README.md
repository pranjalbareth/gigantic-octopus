# Runway

Runway is a Kubernetes operator that gives a developer an isolated, ready-to-use environment from one `kubectl apply`, and deletes it when its time runs out, even if the controller was down at that moment.

```yaml
apiVersion: runway.pranjalbareth.github.io/v1alpha1
kind: Environment
metadata:
  name: pr-1423
spec:
  template: web-small
  ttl: 8h
```

```console
$ kubectl get environments
NAME      TEMPLATE    OWNER              PHASE   EXPIRES
pr-1423   web-small   kubernetes-admin   Ready   2026-11-15T18:00:00Z
```

## Why it exists

Runway v0 ([runway-v0](https://github.com/pranjalbareth/runway-v0)) was a Node.js app that destroyed environments with `setTimeout`. When the process restarted, every timer was gone and expired environments lived forever. The fix there was to check a stored `expires_at` on boot.

That fix is the whole idea behind a Kubernetes controller, so this version is built as one. There is no timer. Expiry is a fact computed from the object (`creationTimestamp + spec.ttl`), and every reconcile checks it. A controller that was down for an hour deletes everything that expired in that hour on its first pass.

In production you would use [vcluster](https://www.vcluster.com/), [Okteto](https://www.okteto.com/) or Argo CD ApplicationSets with pull-request generators. Runway is small on purpose: it shows how a level-triggered controller with finalizers, admission and crash-safe expiry is built, in about 1,200 lines you can read in an afternoon.

## What you get per Environment

The operator creates a namespace `env-<name>` that holds:

| Object | Purpose |
| --- | --- |
| ResourceQuota | Caps CPU, memory and pod count at the template's quota |
| LimitRange | Gives every container a default request and limit, so pods fit the quota |
| NetworkPolicy | Allows ingress only from pods in the same namespace |
| RoleBinding | Gives the owner the built-in `edit` role in this namespace only |
| Deployment + Service | The workload, from the template's image (or `spec.parameters.image`) |

Everything is owned by the Environment. `status` reports `phase`, `expiresAt`, `namespace`, `url` and a `Ready` condition.

## How it works

### The reconcile loop

Every reconcile reads the current state and moves it one step toward the spec. It keeps nothing in memory between calls.

1. Get the Environment. Gone means done.
2. If it is being deleted: delete its namespace, wait until the namespace is fully gone, then remove the finalizer.
3. Make sure the finalizer is set.
4. Compute `expiresAt = creationTimestamp + spec.ttl` and write it to status.
5. If now is at or past `expiresAt`: delete the Environment. Step 2 finishes the job.
6. Create or update the namespace and everything in it.
7. Set the phase from what the Deployment reports.
8. Return `RequeueAfter: expiresAt - now`.

Step 8 is only a wake-up call. If it is lost in a crash, the informer lists every Environment on restart and step 5 catches anything that expired meanwhile.

### Design decisions

- **Extending is a spec change, not an annotation.** To extend, raise `spec.ttl`. An "extend by 4h" annotation that the controller reads and removes would be a one-shot event hidden inside a design that is supposed to react only to current state.
- **Both CRDs are cluster-scoped.** A namespaced object cannot own a Namespace through an ownerReference. A cluster-scoped Environment can.
- **The finalizer makes deletion foreground.** Owner references alone would garbage-collect the namespace in the background. The finalizer deletes it and waits, so when `kubectl delete environment` returns the name is free and nothing is still running.
- **A namespace that already exists is never adopted.** If `env-<name>` exists and is not owned by this Environment, Runway reports `NamespaceConflict` and touches nothing. Adopting it would delete someone else's workloads at expiry.
- **The owner is the authenticated user.** The webhook sets `spec.owner` from the API server's `userInfo`, rejects any other value, and a CEL rule makes it immutable. Nobody can use up another user's limit.
- **The controller holds `bind`, not `edit`.** Kubernetes refuses to let a controller grant permissions it does not have. Instead of giving the controller `edit` everywhere, it gets the `bind` verb on the `edit` ClusterRole only.
- **Time comes from an injected clock.** The reconciler never calls `time.Now()`, so tests step a fake clock instead of sleeping.

### Admission: two mechanisms, on purpose

| Rule | Where | Why there |
| --- | --- | --- |
| Name is 3-40 characters with no dots; TTL between 1m and 168h; owner and template immutable | ValidatingAdmissionPolicy (CEL), `config/admission/` | Needs only the object. Runs inside the API server: no network hop, no outage mode |
| Template exists; TTL fits the template (`maxTTL` on create, `maxLifetime` on extend); at most 3 Environments per owner | Validating webhook (Go) | Needs to read other objects |
| TTL defaults to 24h; owner set from the caller | Mutating webhook (Go) | Needs the request's user |
| A template in use cannot be deleted | Validating webhook on EnvironmentTemplate | Needs to list Environments |

The webhooks use `failurePolicy: Fail`: if they are down, new Environments are refused instead of slipping through unchecked. They are registered for create and update only, and they let through any object that is already being deleted, so a webhook outage never blocks cleanup.

### Metrics

| Metric | Meaning |
| --- | --- |
| `runway_environments{phase}` | Environments by phase, counted from the cache at scrape time |
| `runway_time_to_ready_seconds` | Histogram: creation until the workload is available |
| `runway_expiry_lag_seconds` | Histogram: how late an expiry was acted on. Near zero when healthy; grows by the length of an outage |

Reconcile errors, queue depth and latency come from controller-runtime's built-in `controller_runtime_*` metrics. Every phase change that matters (`Ready`, `TTLChanged`, `Expired`) is also a Kubernetes Event on the Environment.

## Run it

You need Go, Docker, kubectl and [kind](https://kind.sigs.k8s.io/).

```sh
kind create cluster --name runway
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=180s

make docker-build IMG=runway:dev
kind load docker-image runway:dev --name runway
make deploy IMG=runway:dev

kubectl apply -f config/samples/runway_v1alpha1_environmenttemplate.yaml
kubectl apply -f config/samples/runway_v1alpha1_environment.yaml
kubectl get environments -w
```

Reach the workload:

```sh
kubectl port-forward -n env-pr-1423 service/pr-1423 8080:80
```

Extend it, then delete it:

```sh
kubectl patch environment pr-1423 --type merge -p '{"spec":{"ttl":"12h"}}'
kubectl delete environment pr-1423
```

### The crash demo

```sh
kubectl apply -f - <<EOF
apiVersion: runway.pranjalbareth.github.io/v1alpha1
kind: Environment
metadata: {name: crash-demo}
spec: {template: web-small, ttl: 2m}
EOF
kubectl scale deployment runway-controller-manager -n runway-system --replicas=0
sleep 180                       # the TTL runs out with nothing watching
kubectl get environment crash-demo   # still there: nobody is home
kubectl scale deployment runway-controller-manager -n runway-system --replicas=1
kubectl get environment crash-demo -w   # deleted on the first reconcile
```

## Tests

```sh
make test       # unit + envtest (a real API server and etcd, no nodes)
make test-e2e   # kind: deploys the operator and runs the crash and failover tests
make lint
```

| Layer | Where | What it proves |
| --- | --- | --- |
| Unit | `internal/controller/ttl_test.go` | Expiry maths, including the exact boundary |
| Reconciler | `internal/controller/environment_controller_test.go` | Provisioning, idempotence, expiry, restart safety, extension, never adopting a namespace. Uses a fake clock |
| Wiring | same file, "under a manager" | The requeue timer and the three watches work end to end with a real clock |
| Admission | `internal/webhook/v1alpha1/` | Every webhook rule and the shipped CEL policy, against a real API server, as different users |
| e2e | `test/e2e/e2e_test.go` | On kind: kill the controller before a 2m TTL runs out, wait, restart, and assert the environment and namespace are gone. Also kills the leader of two replicas |

## Code map

Read in this order. The first four files are the core.

| File | Lines | What it holds |
| --- | --- | --- |
| `internal/controller/ttl.go` | 45 | The definition of expiry |
| `internal/controller/environment_controller.go` | 360 | The reconcile loop, the finalizer, status, watches |
| `internal/webhook/v1alpha1/environment_webhook.go` | 180 | Defaulting and cross-object validation |
| `api/v1alpha1/environment_types.go` | 170 | The Environment API |
| `internal/controller/resources.go` | 165 | What each owned object looks like |
| `internal/webhook/v1alpha1/environmenttemplate_webhook.go` | 110 | Template checks and the in-use guard |
| `internal/controller/metrics.go` | 90 | The three metrics |
| `config/admission/environment_policy.yaml` | 35 | The CEL rules |

Everything else (`cmd/`, `config/`, `Makefile`, `Dockerfile`, `.github/`) is kubebuilder scaffolding.

## Limits

- **The per-owner limit is not atomic.** Two creates by the same user at the same instant can both pass the count. Closing that gap needs a reservation object or a ResourceQuota on the CRD's object count.
- **Clock skew moves expiry.** `creationTimestamp` is stamped by the API server and "now" is read by the controller. If their clocks differ by five seconds, expiry is five seconds off.
- **Expiry needs the API server.** If it is down at `expiresAt`, deletion happens when it is back.
- **The URL is in-cluster only.** There is no ingress or Gateway route; use `kubectl port-forward`.
- **One workload shape.** A template is one Deployment and one Service.
- **NetworkPolicy needs a CNI that enforces it.** kind's default CNI (kindnet) does in recent versions; some others do not.

## License

Apache 2.0.
