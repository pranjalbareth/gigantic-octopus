# Demo script

About 12 minutes. Every command here is meant to be pasted as is.

## Before the audience arrives (10 minutes)

Start from a clean cluster. Objects were renamed since the last deploy, so do
not deploy over the old one.

```sh
kind delete cluster --name runway
kind create cluster --name runway
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=180s

make docker-build IMG=runway:dev
kind load docker-image runway:dev --name runway
make deploy IMG=runway:dev
kubectl rollout status deployment/runway-controller-manager -n runway-system
kubectl rollout status deployment/runway-portal-server -n runway-system

kubectl apply -f config/samples/runway_v1alpha1_environmenttemplate.yaml
make build-plugin && sudo cp bin/kubectl-runway /usr/local/bin/
```

Pre-pull the workload image so the first environment is Ready in seconds:

```sh
docker pull nginx:1.27 && kind load docker-image nginx:1.27 --name runway
```

Open three terminals and two browser windows (one normal, one private):

| Where | What |
| --- | --- |
| Terminal 1 | `kubectl port-forward -n runway-system service/runway-portal 8090:80` (leave running) |
| Terminal 2 | `kubectl get environments -w` (leave running) |
| Terminal 3 | Your commands |
| Browser A | http://localhost:8090, sign in as `alice` |
| Browser B (private window) | http://localhost:8090, sign in as `bob` |

Rehearse once, then delete what you made: `kubectl delete environments --all`.

## 1. The problem (1 minute, no typing)

"Version 0 destroyed environments with a timer in memory. Restart the
process and the timers were gone; expired environments lived forever. This
version has no timer. Expiry is a fact about the object, and every reconcile
checks it."

## 2. Self-service (2 minutes) — Browser A, alice

1. Start an environment: name `pr-1423`, template `web-small`, lifetime `8h`.
2. Point at Terminal 2: the object appeared, with `OWNER dev:alice`. "The
   portal did not write that. The webhook did, from the user the API server
   authenticated."
3. The card goes Provisioning then Ready without a reload. "That is a
   Kubernetes watch, pushed to the browser."
4. Terminal 3, show what one object produced:

```sh
kubectl get all,resourcequota,limitrange,networkpolicy,rolebinding -n env-pr-1423
```

5. Copy the port-forward command from the card, run it, open http://localhost:8080.

## 3. Guard rails (3 minutes)

Browser B, bob: his list is empty. He cannot see alice's environment.

Terminal 3, bob tries anyway, through the API:

```sh
kubectl delete environment pr-1423 --as=dev:bob
kubectl patch environment pr-1423 --type merge -p '{"spec":{"ttl":"100h"}}' --as=dev:bob
```

Both are refused with "only the owner (dev:alice) can change or delete this
environment". "That rule is 6 lines of CEL inside the API server. The portal
has no authorization code at all."

Browser A, alice: start three more (`a-1`, `a-2`, `a-3`). The fourth is
refused with the webhook's message, shown in the page.

Then a few fast refusals in the form: lifetime `30s`, lifetime `100h`, name `pr.1`.

Clean up two: click Delete on `a-1` and `a-2`.

## 4. The crash (4 minutes) — the point of the project

Browser A: start `crash-demo` with lifetime `2m`. Wait for Ready.

```sh
kubectl scale deployment runway-controller-manager -n runway-system --replicas=0
kubectl get pods -n runway-system        # only the portal is left
```

While the two minutes run out, talk:

- "Nothing is counting down. The card's clock is the browser doing subtraction."
- Try to start an environment in the portal now: refused. "The webhook is
  part of the controller, and it fails closed."
- When the card says "Time is up": `kubectl get environment crash-demo`. Still
  there, still Ready. "Nobody is home to delete it."

```sh
kubectl scale deployment runway-controller-manager -n runway-system --replicas=1
```

Watch Terminal 2 and the card: Expiring, Deleting, gone, with the namespace.
"The first thing the new process did was list every environment and compare
`creationTimestamp + ttl` with now. It did not need to remember anything."

Show how late it was:

```sh
kubectl get events --field-selector reason=Expired --sort-by=.lastTimestamp | tail -1
```

## 5. Same thing from the terminal (1 minute)

```sh
kubectl runway list --all
kubectl runway extend pr-1423 4h
kubectl runway describe pr-1423
```

The card in Browser A moves its expiry without a reload.

## 6. Close (1 minute)

"About 2,250 lines of Go. The operator is the only thing that changes the
cluster. Kubernetes is the only database. The portal can be deleted and
nothing is lost." If asked what you would change: the per-owner limit is a
count followed by a create, so two simultaneous requests can both pass.

## If something goes wrong

| Symptom | Fix |
| --- | --- |
| Portal page does not load | Terminal 1's port-forward died; run it again |
| "connection refused" creating anything | Controller is still starting or scaled to 0: `kubectl get pods -n runway-system` |
| Environment stuck in Provisioning | Image pull: `kubectl get pods -n env-<name>`; pre-load the image as above |
| Environment stuck in Deleting | A pod is slow to stop; it finishes within the pod's grace period (30s) |
| Portal shows nothing after controller restart | Reload the page once; the event stream reconnects by itself within seconds |
| You are out of time | Skip section 3's extra refusals and section 5 |

## Questions you will get

- **Why not just owner references?** They delete in the background. The
  finalizer makes deletion foreground: the name is free when the delete returns.
- **What if the API server is down at expiry?** Deletion happens when it is
  back. Expiry lag is a metric: `runway_expiry_lag_seconds`.
- **Why can bob list alice's environment with kubectl?** RBAC cannot filter a
  list by a field. Reads are open, writes are owner-only.
- **Is the demo login secure?** No, and the yellow bar says so. The GitHub
  overlay puts oauth2-proxy in front; see docs/github-login.md.
- **Why impersonation?** So the portal cannot do anything the user could not
  do with kubectl. Its service account has one permission.
