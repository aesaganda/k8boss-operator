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

```sh
make bundle-build bundle-push bundle-run BUNDLE_IMG=<your-registry>/k8boss-operator-bundle:v0.1.0
```

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

## Develop

```sh
make build vet test      # test needs envtest assets; the target provisions them
make manifests generate  # regenerate CRDs and deepcopy from the Go types
make bundle              # regenerate + validate the OLM bundle
```

`bundle/` is generated from `config/` and is what OLM grants permissions from,
so it is diff-gated in CI: a stale bundle does not fail loudly, it hands OLM
the wrong RBAC.

## Design commitments

- **No edge without evidence.** Every graph edge records why it exists, where
  that came from, and how sure it is.
- **Time is a property of the edge.** Deleting a CR *closes* its edges through
  a finalizer; nothing is hard-deleted, so history stays queryable.
- **Say which question you failed to answer.** A code path that cannot
  determine something reports that, rather than returning a confident zero.

## Licence

Apache-2.0. See [LICENSE](LICENSE).
