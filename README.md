# Runway

Runway is a small Kubernetes-native developer platform. A developer asks for an isolated, ready-to-use environment from a web page or with one `kubectl apply`, and Runway deletes it when its time runs out, even if the controller was down at that moment.

It has three parts:

- **The operator** creates, repairs and deletes everything. It is the only component that changes the cluster.
- **Admission** (two CEL policies and two webhooks) decides what is allowed: limits, ownership, valid templates.
- **The portal** is a web page over the Kubernetes API. It stores nothing and holds no permissions except "act as the signed-in user".

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

In production you would use [vcluster](https://www.vcluster.com/), [Okteto](https://www.okteto.com/) or Argo CD ApplicationSets with pull-request generators, with [Backstage](https://backstage.io/) in front. Runway is small on purpose: it shows how a level-triggered controller with finalizers, admission, crash-safe expiry and a stateless portal is built, in about 2,250 lines of Go you can read in a day.

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
- **Only the owner can change or delete an Environment.** A CEL policy compares the caller with `spec.owner`. The controller and cluster admins are exempt because they hold a custom `administer` verb, which the policy checks by asking the API server's authorizer. No service-account name is written into the policy, and deletion never depends on a webhook.
- **Everything owned is watched.** Delete the Service or the NetworkPolicy by hand and the next reconcile puts it back.
- **Time comes from an injected clock.** The reconciler never calls `time.Now()`, so tests step a fake clock instead of sleeping.

### Admission: two mechanisms, on purpose

| Rule | Where | Why there |
| --- | --- | --- |
| Name is 3-40 characters with no dots; TTL between 1m and 168h; owner and template immutable | ValidatingAdmissionPolicy (CEL), `config/admission/` | Needs only the object. Runs inside the API server: no network hop, no outage mode |
| Only the owner (or an administrator) may update or delete | ValidatingAdmissionPolicy (CEL + authorizer check) | Needs only the object and the caller. Covers DELETE without a webhook in the deletion path |
| Template exists; TTL fits the template (`maxTTL` on create, `maxLifetime` on extend); at most 3 Environments per owner | Validating webhook (Go) | Needs to read other objects |
| TTL defaults to 24h; owner set from the caller | Mutating webhook (Go) | Needs the request's user |
| A template in use cannot be deleted | Validating webhook on EnvironmentTemplate | Needs to list Environments |

The webhooks use `failurePolicy: Fail`: if they are down, new Environments are refused instead of slipping through unchecked. They are registered for create and update only, and they let through any object that is already being deleted, so a webhook outage never blocks cleanup.

### The portal

```
browser -> (oauth2-proxy: GitHub login) -> portal -> Kubernetes API as that user
                                                      -> CEL policies -> webhooks -> stored
                                                      operator reconciles -> watch -> Server-Sent Event -> browser refreshes
```

- **Impersonation, not privileges.** The portal's service account can do one thing: impersonate users. Every call is made as the signed-in person, so RBAC, the policies and the webhooks apply exactly as they do to kubectl. The portal contains no authorization code.
- **No database.** Every page is a List; every button is a Create, Patch or Delete of an Environment. Kill the portal mid-request and nothing is lost.
- **Live updates.** Each browser holds one event stream backed by a watch. An event means "something changed": the page fetches the list again. When the API server ends the watch, a new one starts and one catch-up event is sent.
- **Usernames are prefixed** (`github:alice`, `dev:alice`). Without the prefix, someone with the GitHub login `kubernetes-admin` would become the cluster admin.
- **Two sign-in modes.** `--auth=dev` (the default deploy) lets anyone pick a name, for demos. `--auth=proxy` trusts oauth2-proxy's headers; the shipped overlay runs the proxy as a sidecar and binds the portal to 127.0.0.1, so nothing else can send those headers. See [docs/github-login.md](docs/github-login.md).
- **Cross-site requests are refused.** Every state-changing request must carry a header that only the page's own script can add.

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

make docker-build IMG=runway:dev      # one image: the controller and the portal
kind load docker-image runway:dev --name runway
make deploy IMG=runway:dev

kubectl apply -f config/samples/runway_v1alpha1_environmenttemplate.yaml
kubectl apply -f config/samples/runway_v1alpha1_environment.yaml
kubectl get environments -w
```

Open the portal at http://localhost:8090 and sign in with any name:

```sh
kubectl port-forward -n runway-system service/runway-portal 8090:80
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

### The kubectl plugin

```sh
make build-plugin && sudo cp bin/kubectl-runway /usr/local/bin/
kubectl runway list              # yours, soonest to expire first
kubectl runway extend pr-1423 4h
kubectl runway describe pr-1423
```

### Helm

`dist/chart` is generated from `config/` (`make helm-chart`). It installs the same objects as `make deploy`, into a namespace you create:

```sh
helm install runway dist/chart -n runway-system --create-namespace \
  --set manager.image.repository=runway --set manager.image.tag=dev
```

A full demo script is in [docs/DEMO.md](docs/DEMO.md).

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
| Admission | `internal/webhook/v1alpha1/` | Every webhook rule and both shipped CEL policies, against a real API server, as ordinary users with only the shipped RBAC |
| Portal | `internal/portal/` | The whole product in one process (API server, controller, webhooks, policies, portal). The portal runs with only its shipped ClusterRole. Covers per-user views, owner-only actions, the limit, error messages, CSRF, the event stream, proxy sign-in, and the kubectl plugin binary |
| Plugin | `internal/cli/` | Table output, sorting and extension maths |
| e2e | `test/e2e/e2e_test.go` | On kind: kill the controller before a 2m TTL runs out, wait, restart, and assert the environment and namespace are gone; fail closed while the webhook is down; owner RBAC boundaries; the portal serving; killing the leader of two replicas |

## Code map

Read in this order. The first five files are the core.

| File | Lines | What it holds |
| --- | --- | --- |
| `internal/controller/ttl.go` | 45 | The definition of expiry |
| `internal/controller/environment_controller.go` | 370 | The reconcile loop, the finalizer, status, watches |
| `internal/webhook/v1alpha1/environment_webhook.go` | 180 | Defaulting and cross-object validation |
| `config/admission/environment_policy.yaml` | 70 | The CEL rules, including ownership |
| `internal/portal/kube.go` | 120 | Per-user impersonating clients; turning API errors into sentences |
| `internal/portal/server.go` | 450 | Routes, pages, actions and the event stream |
| `internal/portal/auth.go` | 95 | The two sign-in modes and why usernames are prefixed |
| `api/v1alpha1/environment_types.go` | 170 | The Environment API |
| `internal/controller/resources.go` | 160 | What each owned object looks like |
| `internal/webhook/v1alpha1/environmenttemplate_webhook.go` | 110 | Template checks and the in-use guard |
| `internal/controller/metrics.go` | 90 | The three metrics |
| `internal/portal/templates/`, `static/` | 375 | The page: HTML, CSS, a little JavaScript (htmx and the Barlow fonts are vendored) |
| `cmd/kubectl-runway/`, `internal/cli/` | 250 | The kubectl plugin |

Everything else (`cmd/main.go`, most of `config/`, `Makefile`, `Dockerfile`, `.github/`, `dist/chart`) is kubebuilder scaffolding or generated.

## Limits

- **The per-owner limit is not atomic.** Two creates by the same user at the same instant can both pass the count. Closing that gap needs a reservation object or a ResourceQuota on the CRD's object count.
- **Clock skew moves expiry.** `creationTimestamp` is stamped by the API server and "now" is read by the controller. If their clocks differ by five seconds, expiry is five seconds off.
- **Expiry needs the API server.** If it is down at `expiresAt`, deletion happens when it is back.
- **Everyone can list every Environment.** RBAC cannot filter a list by owner. The portal shows you only yours, but `kubectl get environments` shows all names. Changing or deleting someone else's is refused.
- **Dev sign-in is not authentication.** `--auth=dev` is for demos. Use the GitHub overlay anywhere other people can reach the portal.
- **Egress is open.** The NetworkPolicy isolates ingress only, so workloads can still call out (image pulls, APIs). Lock egress per template if your cluster needs it.
- **The URL is in-cluster only.** There is no ingress or Gateway route; use `kubectl port-forward`.
- **One workload shape.** A template is one Deployment and one Service.
- **NetworkPolicy needs a CNI that enforces it.** kind's default CNI (kindnet) does in recent versions; some others do not.

## License

Apache 2.0.
