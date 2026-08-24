# Roadmap — idp-operator

Four milestones, each sized to roughly one weekend. A milestone is done when its exit
criteria pass, not when the code exists. Status today: the golden-path reconciler
(namespace → SLO baseline → PodDisruptionBudget → ArgoCD Application), spec validation
with status reporting, and fake-client tests at 90% controller coverage.

## M1 — correctness hardening

The reconciler creates objects; it doesn't yet own their whole lifecycle.

- [ ] Finalizer on `ServiceDeployment` + garbage collection of everything it provisioned
      (delete the manifest, the golden path disappears — no orphaned namespaces)
- [ ] OwnerReferences on provisioned objects where namespace boundaries allow
- [ ] `controller-gen`: generated deepcopy + CRD manifests replace the hand-written ones
- [ ] envtest suite alongside the fake-client tests — reconcile against real API-server
      semantics (conflicts, resourceVersion, deletion timestamps)

**Exit:** create → verify → delete round-trip green in envtest; `go vet` and
`controller-gen` output committed and in sync.

## M2 — policy baseline

Provisioning should hand out guardrails, not just resources.

- [ ] NetworkPolicy default-deny (+ DNS egress allowance) per provisioned namespace
- [ ] Kyverno policy baseline: signed-image verification (Cosign) and disallowed
      privileged pods, installed as part of the golden path
- [ ] Status conditions per provisioned component (`NamespaceReady`, `PolicyReady`, ...)
      instead of a single ready flag

**Exit:** a fresh `ServiceDeployment` yields a namespace where an unsigned or
privileged pod is rejected out of the box, and `.status.conditions` says why.

## M3 — budget-aware delivery (the point of the project)

Integrate [error-budget-gate](https://github.com/JIUNG9/error-budget-gate) so the SLO
baseline is enforced, not just declared.

- [ ] Import budgetgate as a library; feed it the service's SLO baseline + burn input
- [ ] Gate the ArgoCD Application sync policy on the verdict:
      `allow` → automated sync · `require-human` → manual sync + condition ·
      `block` → sync disabled + condition
- [ ] Burn-rate input pluggable (Prometheus query to start; interface for others)
- [ ] Tests for all three verdicts, including verdict flapping

**Exit:** demo scenario in envtest — a budget-exhausted service loses automated sync
and the `ServiceDeployment` status explains it.

## M4 — ship

- [ ] kind-based e2e: quickstart from the README executed by a script, asserting the
      full golden path (used as CI's e2e job and as the demo)
- [ ] CI workflow: build, vet, test (+ e2e on label), coverage badge
- [ ] README quickstart verified verbatim on a clean machine; demo GIF
- [ ] Example gallery: minimal, full-spec, and budget-gated `ServiceDeployment`s
- [ ] Final review of history and tree for anything non-public, then flip to public

**Exit:** public repo, CI green on the first public commit.

## Non-goals (for now)

Multi-cluster placement, a UI, Backstage plugin, and template scaffolding of the app
repo itself. The scope is the reliability golden path: provisioning with SLOs,
disruption budgets, policy, and budget-gated delivery as defaults.
