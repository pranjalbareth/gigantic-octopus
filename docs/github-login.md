# GitHub sign-in for the portal

The default deploy uses demo sign-in. This overlay replaces it with GitHub
login through [oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/).

Status: the manifests build and the portal's proxy mode is covered by tests,
but this overlay has not been run end to end against GitHub. Try it before
relying on it.

## 1. Create a GitHub OAuth app

GitHub > Settings > Developer settings > OAuth Apps > New OAuth App.

| Field | Value |
| --- | --- |
| Homepage URL | `http://localhost:4180` |
| Authorization callback URL | `http://localhost:4180/oauth2/callback` |

Copy the client ID and generate a client secret.

## 2. Create the secret

```sh
kubectl create namespace runway-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic runway-portal-oauth -n runway-system \
  --from-literal=OAUTH2_PROXY_CLIENT_ID=<client id> \
  --from-literal=OAUTH2_PROXY_CLIENT_SECRET=<client secret> \
  --from-literal=OAUTH2_PROXY_COOKIE_SECRET="$(openssl rand -base64 32 | tr -- '+/' '-_')"
```

## 3. Deploy with the overlay

```sh
make docker-build IMG=runway:dev && kind load docker-image runway:dev --name runway
(cd config/manager && ../../bin/kustomize edit set image controller=runway:dev)
(cd config/portal && ../../bin/kustomize edit set image controller=runway:dev)
bin/kustomize build config/github-login | kubectl apply -f -
kubectl port-forward -n runway-system service/runway-portal 4180:80
```

Open http://localhost:4180. After GitHub sends you back, you are
`github:<your login>` to Kubernetes.

## How it is wired

- oauth2-proxy runs as a sidecar in the portal pod and is the only container
  with a port on the pod's network.
- The portal listens on `127.0.0.1:8080`. Nothing outside the pod can reach
  it, so the identity headers it trusts can only come from the proxy.
- To restrict who may sign in, add `--github-org=<org>` or `--github-user=<a,b>`
  to the proxy's args in `config/github-login/portal_patch.yaml`.
- Behind real TLS, set `--cookie-secure=true` and change `--redirect-url`.
