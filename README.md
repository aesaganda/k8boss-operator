# K8Boss Operator

Kubernetes-native intent layer for [K8Boss](https://github.com/aesaganda/k8boss).
It turns three CRDs into reconciled, evidence-backed facts in the K8Boss
Knowledge Graph.

| Kind | Scope | What it declares |
|---|---|---|
| `SegmentationPolicy` | Namespaced | Network segmentation intent for a set of workloads |
| `RuntimeSecurityPolicy` | Namespaced | Attaches Knowledge Graph evidence to an existing Tetragon `TracingPolicy` |
| `PlatformConfig` | Cluster | Singleton: selects the flow telemetry provider, holds the kill switch |

## This operator requires a K8Boss control plane, and does not install one

It is a **bridge, not an installer**. Every reconcile is one authenticated HTTP
call to a K8Boss backend that must already be running; the operator writes
nothing to a database directly and creates no workloads — it has no RBAC to do
so.

The operator is open source (Apache-2.0). The K8Boss control plane it talks to
is a separate, commercially licensed product.

Installed without a control plane, the operator comes up **healthy and idle**
and says so on each CR's status conditions rather than pretending to work. It
deliberately distinguishes `AwaitingConfiguration` ("nobody has told us where
the backend is") from `BackendUnreachable` ("we tried and could not reach it").
Reporting the second as the first would be a claim the system cannot support.

## Install

### With OLM

The published image is `ghcr.io/aesaganda/k8boss-operator`, built for
`linux/amd64` and `linux/arm64`. To try a bundle from a local checkout against
a cluster that has OLM:

```sh
make bundle-build bundle-push bundle-run BUNDLE_IMG=<your-registry>/k8boss-operator-bundle:v0.1.0
```

An OLM install carries no user input, so the operator comes up **unconfigured**
and stays healthy: readiness passes (there is no outage to report), nothing is
reconciled, and every CR reports `Ready=False` with reason
`AwaitingConfiguration` naming the variables to set. See [Configure](#configure).

`installModes` is `AllNamespaces` only: `PlatformConfig` is cluster-scoped and
the manager has no per-namespace watch handling, so it always watches
cluster-wide. Advertising `SingleNamespace` would claim a confinement the
operator does not honour.

### Directly

```sh
make install   # CRDs only
make deploy    # CRDs + RBAC + ServiceAccount + Deployment
```

## Configure

Three settings, all optional at startup — the operator runs unconfigured and
tells you what is missing:

| Setting | Meaning |
|---|---|
| `K8BOSS_BACKEND_URL` | The K8Boss backend Service URL |
| `K8BOSS_CLUSTER_ID` | This cluster's registration id, from the K8Boss UI |
| `K8BOSS_OPERATOR_TOKEN` | Must match `OPERATOR_API_TOKEN` on the backend; read from the `k8boss-operator` Secret, key `token` |

Under OLM, set them via the Subscription's `spec.config.env`.

Then apply a `PlatformConfig` named `default`. **`spec.paused` defaults to
`true`** so an operator with no configuration cannot silently begin mutating
cluster state — creating the CR with `paused: false` is the deliberate act that
enables reconciliation.

```sh
kubectl apply -f config/samples/
```

## Behind a cluster-wide proxy

The operator honours `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`, which OLM
injects into the manager Deployment when the cluster has a proxy configured. It
does nothing special to get this: the HTTP client leaves `Transport` nil, so it
uses Go's `http.DefaultTransport` and its `ProxyFromEnvironment`.

**The trap is `NO_PROXY`, and it is worth knowing before you hit it.** The
backend is normally an in-cluster Service
(`http://k8boss-backend.k8boss-system.svc:8010`), so unless `NO_PROXY` covers
`.svc` and `.cluster.local` the operator will send that in-cluster call to an
*egress* proxy, which has no route to it. OpenShift's generated `NO_PROXY`
already includes both; a hand-set proxy environment on vanilla Kubernetes often
does not. The symptom is `Ready=False` with a transport error — correctly
reported as unreachable rather than as unconfigured, but the cause is the proxy,
not the backend.

## What this operator claims on OperatorHub

The CSV carries the ten `features.operators.openshift.io/*` labels that
`community-operators-prod` requires. They are claims on a public listing, so
each is answered from the code rather than copied from another bundle:

| Claim | | Why |
|---|---|---|
| `disconnected` | **true** | At runtime it opens one kind of connection: HTTP to the backend URL you supply. Nothing is fetched from the internet, and the single image it runs is declared in `spec.relatedImages` for `oc adm catalog mirror`. |
| `proxy-aware` | **true** | See above; pinned by `TestNewUsesProxyHonouringTransport`. |
| `fips-compliant` | **false** | Built `CGO_ENABLED=0` against upstream `golang` onto `distroless/static`. FIPS needs a certified crypto module — a RHEL base and the Red Hat Go toolchain. Not a label we can flip. |
| `tls-profiles` | **false** | Does not read the cluster `tlsSecurityProfile`; serves no TLS of its own (metrics bind to `127.0.0.1`). |
| `token-auth-aws` / `-azure` / `-gcp` | **false** | No cloud integration. It authenticates to exactly one thing, with a bearer token from a Secret. |
| `cnf` / `cni` / `csi` | **false** | Not a network function, not a CNI plugin, not a CSI driver. The CNI line is not pedantry: this operator records segmentation **intent** and never enforces it — enforcement is the CNI's job. |

`com.redhat.openshift.versions` is `v4.14`, meaning 4.14 and above. That is a
*tested floor*, not an assertion that 4.13 fails.

## Develop

```sh
make build vet test      # test needs envtest assets; the target provisions them
make manifests generate  # regenerate CRDs and deepcopy from the Go types
make bundle              # regenerate + validate the OLM bundle
```

`bundle/` is generated from `config/` and is what OLM grants permissions from,
so it is diff-gated in CI: a stale bundle does not fail loudly, it hands OLM
the wrong RBAC.

### Cutting a submission

The checked-in bundle names the floating `:latest` tag, which is right for
`make deploy` and wrong for a submission — a CSV that claims to describe a
specific operator must not name a tag that can be repointed tomorrow, and a
floating tag cannot be mirrored reproducibly. `make bundle-submission` refuses
anything but a digest:

```sh
git tag v0.1.0 && git push origin v0.1.0    # publish job prints the digest
make bundle-submission OPERATOR_IMG=ghcr.io/aesaganda/k8boss-operator@sha256:<digest>
```

Then copy `bundle/manifests` and `bundle/metadata` into
`operators/k8boss-operator/<version>/` in a fork of
[k8s-operatorhub/community-operators](https://github.com/k8s-operatorhub/community-operators)
(OperatorHub.io) and/or
[redhat-openshift-ecosystem/community-operators-prod](https://github.com/redhat-openshift-ecosystem/community-operators-prod)
(the OpenShift embedded OperatorHub). They are separate PRs to separate
repositories. Each must be DCO-signed, one squashed commit, touching nothing
outside `operators/`. Bump `VERSION` and `createdAt` every time — re-pushing an
existing version is rejected.

Note that `bundle-submission` leaves the digest in
`config/manifests/kustomization.yaml`; run plain `make bundle` afterwards to
restore the floating-tag default before committing.

## Design commitments

- **No edge without evidence.** Every graph edge records why it exists, where
  that came from, and how sure it is.
- **Time is a property of the edge.** Deleting a CR *closes* its edges through
  a finalizer; nothing is hard-deleted, so history stays queryable.
- **Say which question you failed to answer.** A code path that cannot
  determine something reports that, rather than returning a confident zero.

## Security

Please do not open a public issue for a vulnerability. See
[SECURITY.md](SECURITY.md).

## Contributing

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).
Commits must be [DCO](https://developercertificate.org/)-signed (`git commit -s`),
because that is what the OperatorHub submission requires of anything that ends
up in the bundle.

## Licence

Apache-2.0. See [LICENSE](LICENSE).
