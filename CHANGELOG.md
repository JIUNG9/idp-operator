# Changelog

Notable changes per milestone. Versions are `v0.x` while the API is `v1alpha1`
and may still change.

## Unreleased

- `WorkloadsSelected` now also checks the labels of running pods owned by a ReplicaSet or StatefulSet.
  Before, it read only the pod template, so a pod created before the template was fixed kept its old
  labels, stayed outside the PDB, and the condition read `True`. The condition now reads `LabelMissing`
  until that pod is replaced. Pods owned by Jobs or other controllers and pods that finished or are
  terminating are skipped. The message names at most 5 workloads and says "and N more" after that, and
  it now reads "is missing from the pod labels" (was "pod template lacks").
- The operator now has leader election, health probes and a metrics endpoint. New flags:
  `-leader-elect` (default off), `-metrics-bind-address` (default `:8080`) and
  `-health-probe-bind-address` (default `:8081`, serves `/healthz` and `/readyz`).
  `config/manager/manager.yaml` passes `-leader-elect`, exposes both ports and adds liveness and
  readiness probes. New `config/rbac/leader_election_role.yaml` grants the Lease and event
  permissions in `idp-system`; the kustomization includes it. Before this, a second replica would
  have reconciled in parallel, and a hung process was not restarted. `/readyz` is a ping check, so it
  does not yet report whether the cache has synced.

- The controller no longer reconciles on its own status writes. The `ServiceDeployment` watch passes
  spec changes (generation), the start of deletion and creates; the requeue timers still drive the
  budget gate and the Kyverno retry. The standing-verdict message on `BudgetGateReady` no longer
  contains the age of the reading ("5s ago"), which changed on every reconcile and forced a status
  write each time. New `Ready` condition: `False` with reason `Degraded` while a provisioned component
  is not operative (the budget gate holding delivery). `Provisioned` keeps meaning "the objects exist",
  so its reason is no longer `Degraded`; it is `Reconciled` or `ReconciledWithSkips`. Deliberate skips
  do not turn `Ready` false.

- Deleting a ServiceDeployment no longer deletes the team's namespace by default. New
  `spec.deletionPolicy` (`Retain` or `Delete`, default `Retain`): `Retain` leaves the namespace, its
  workloads and PersistentVolumeClaims in place and still deletes the ArgoCD Application; `Delete`
  keeps the old behaviour. Existing objects get `Retain` from the CRD default, so the cascade is now
  opt-in. The Backstage template has a matching field.

- `spec.gitops.project` is checked against a new `-allowed-projects` flag. The operator writes
  ArgoCD Applications with its own privileges, so a team that could name any AppProject could deploy
  with the permissions of the most privileged one. A ServiceDeployment whose project is not listed
  (an omitted project counts as `default`) gets phase `Invalid`, condition `Provisioned=False` with
  reason `ProjectNotAllowed`, and no Application is written. The flag has no default: with it unset
  every ServiceDeployment is rejected, and the operator logs that at startup. `config/manager/manager.yaml`
  passes `-allowed-projects=default` for the quickstart. New `-argocd-namespace` flag (default
  `argocd`) sets where Applications are written and deleted.

- The operator no longer adopts, overwrites or deletes an object it did not create. An existing
  ConfigMap, PodDisruptionBudget, NetworkPolicy, Kyverno Policy or ArgoCD Application whose labels
  lack `managed-by=idp-operator` (or whose owner-namespace is another namespace) is left untouched and
  the component's condition becomes `False` with reason `NotOwned`. This also covers cleanup deletes
  (stale PDB, stale platform NetworkPolicy) and teardown of the Application. Previously a same-named
  object was taken over. See "Ownership rule" in the README
- `WorkloadsSelected` condition: lists Deployments and StatefulSets in the service namespace and
  reports any whose pod template lacks `app.kubernetes.io/name: <service>`, the label the PDB
  selects on (`LabelMissing`, degraded). `NoWorkloads` until something is delivered. Omitted when
  the service has no PDB. Adds a `get;list` RBAC rule on `apps` Deployments and StatefulSets
- `WorkloadsSelected` also checks bare Pods (no controller owner) on their own labels. Pods owned by a
  controller are still judged through the owner's template. Adds a `get;list` RBAC rule on `pods`
- `WorkloadsSelected` also reads Argo Rollouts (`spec.template.metadata.labels`) when the Rollout CRD is installed.
  A cluster without it behaves as before: a NotFound or no-match answer means no Rollouts. Any other list error
  (for example Forbidden) fails the reconcile instead of hiding Rollouts. Adds a `get;list` RBAC rule on `rollouts`
- Backstage software template (`templates/backstage/service-deployment/`): a form that opens a PR adding a
  `ServiceDeployment`. `test/scaffolder` fails if the form drifts from the CRD or the skeleton stops rendering a
  valid manifest
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
