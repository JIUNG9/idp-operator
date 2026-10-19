# Changelog

Notable changes per milestone. Versions are `v0.x` while the API is `v1alpha1`
and may still change.

## Unreleased

- e2e: on failure, dump pod descriptions, events, operator logs and the
  ServiceDeployment status before the cluster is deleted, so a CI timeout
  explains itself
- e2e: the probe pods run `registry.k8s.io/pause:3.10`, which kind nodes already
  carry, instead of pulling busybox from a public registry (`ErrImagePull`,
  429 on shared runners). `PROBE_IMAGE` overrides it; the script checks the
  image is on the node first

## v0.5.0 — M4, ship

- Dockerfile (distroless, non-root, static binary)
- kind-based end-to-end test: the README quickstart executed, plus the two
  things envtest cannot prove — that the Kyverno baseline actually rejects a
  privileged pod, and that the PDB really selects a correctly labelled workload
- CI: build, vet, gofmt, generated-output-in-sync check, unit + envtest; e2e on
  demand (PR label `e2e`, or manual dispatch)
- OSS hygiene workflow and spec, so the repo is deployable by anyone with
  config-only changes
- Example gallery: `minimal.yaml`, `orders-api.yaml` (full spec),
  `budget-gated.yaml`

The e2e's first real run found two things, both in the test rather than the
operator, and both confirmed against a live cluster rather than assumed:
Kyverno cannot be installed with a client-side apply (its CRDs exceed the
262144-byte `last-applied-configuration` annotation cap), and the label-contract
assertion read `expectedPods` — which is 0 for a bare pod because it derives
from the scale subresource of a controller that isn't there — instead of
`currentHealthy`, which was 1 throughout.

Still open, and not a code gap: "CI green on the first public commit" cannot be
satisfied by any local action. It waits on the repo going public.

## v0.4.0 — M3, budget-aware delivery

- A service's remaining error budget decides whether ArgoCD may sync it
  unattended: `allow` keeps automated sync, `require-human` and `block` take it
  away
- `internal/budget` shares [error-budget-gate](https://github.com/JIUNG9/error-budget-gate)'s
  verdict vocabulary and thresholds, and diverges on one rule deliberately —
  production alone does not require a human here, because the gated action is a
  reviewed commit rather than a remediation invented by automation. The
  comparison is in the README.
- `BurnSource`: a Prometheus implementation over a configurable query template,
  and a static one for demos. No data, `NaN`, or an error is treated as no
  evidence — never as a healthy budget
- The verdict is persisted in `status.budget` and survives operator restarts.
  Tightening is immediate; relaxing waits for three agreeing readings, paced by
  elapsed time rather than reconcile count
- An unreadable burn source holds the last measured verdict for a bounded grace
  period, then tightens. It never loosens
- `spec.tier` and `spec.environment` added; `spec.replicas` and `spec.tier` are
  pointers so an explicit `0` is not silently replaced by the default

## v0.3.0 — M2, policy baseline

- NetworkPolicies: default-deny ingress, re-admitting the namespace itself and
  the platform's declared ingress sources. Egress is deliberately untouched
- Kyverno baseline as unstructured objects, so Kyverno stays optional: without
  its CRDs the golden path still provisions and the status says the guardrail is
  missing
- Cosign image verification when a signing identity is configured.
  `mutateDigest` is explicitly `false` — Kyverno's default rewrites the image tag
  to a digest at admission, which fights the `selfHeal` ArgoCD Application this
  operator provisions for the same service
- Per-component status conditions with an aggregate that distinguishes a
  deliberate skip from a failure

## v0.2.0 — M1, correctness hardening

- Finalizer-based garbage collection of everything provisioned
- PodDisruptionBudget derived from declared replicas: `maxUnavailable: 1` at two
  or more, none below that plus a condition explaining why
- Defaults and validation moved into the CRD schema; generated deepcopy and CRD
  via `controller-gen`
- Drift detection through label-mapped watches; namespace ownership guards
- RBAC markers and installable manifests; envtest suite

## v0.1.0 — the golden path

One `ServiceDeployment` provisions a namespace, an SLO baseline, a
PodDisruptionBudget, and an ArgoCD Application.
