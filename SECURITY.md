# Security Policy

## Scope

This repository is the **K8Boss operator** only: three CRDs and a controller
manager that translate Kubernetes-native intent into calls against a K8Boss
control-plane API. The control plane itself is a separate, closed-source
product and is **not** in scope here — if the issue is in the backend, the
frontend or the node agent, report it to the address below and say so, but do
not expect a public fix in this repository.

What the operator can actually do, which bounds the impact of a flaw in it:

- It talks to exactly one network endpoint, the backend URL an administrator
  supplies, authenticated with a bearer token from a Secret.
- It **never** writes to a database, and it creates no workloads — it holds no
  RBAC to do so.
- It never enforces network policy. It records *intent*; enforcement is the
  CNI's job.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private reporting
([Security → Report a vulnerability](https://github.com/aesaganda/k8boss-operator/security/advisories/new)),
or email **erensaganda@gmail.com** with `k8boss-operator` in the subject.

Please include the operator version or image digest, the Kubernetes or OpenShift
version, and enough detail to reproduce. A proof of concept is welcome; please
do not test against clusters you do not own.

This is a single-maintainer project, so be realistic about timing: expect an
acknowledgement within **7 days** and an assessment within **30**. If you have
heard nothing after 14 days, send a reminder — it was missed, not ignored.

Please give a fix a reasonable window before disclosing publicly, and tell us if
you have a date in mind. Credit is given by default; say so if you would rather
not be named.

## Supported versions

Only the latest `0.x` release receives fixes. The API is `v1alpha1` and the
bundle ships `maturity: alpha`: the CRD field shapes are a contract, not a final
API, and may change between minor versions.

| Version | Supported |
|---|---|
| latest `0.x` | yes |
| anything older | no |

## What is not a vulnerability

These are documented behaviour, deliberately chosen, and reporting them is
welcome as a normal issue rather than a security report:

- **An unconfigured operator reports itself Ready.** It has no backend to be
  unreachable from, reconciles nothing, and says
  `Ready=False reason=AwaitingConfiguration` on every CR. Degrading readiness
  for an outage that does not exist is the failure mode this avoids.
- **`spec.paused` defaults to `true`.** The reconcilers refuse to mutate
  anything while they cannot positively confirm the kill switch is off. This is
  fail-closed on purpose.
- **The manager is cluster-scoped.** `installModes` advertises `AllNamespaces`
  only, precisely so that no one is told the operator is confined to a namespace
  while it watches the whole cluster.
