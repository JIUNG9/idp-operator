# Roadmap — idp-operator

Four milestones, each sized to roughly one weekend. A milestone is done when its exit
criteria pass, not when the code exists. Status today: the golden-path reconciler
(namespace → SLO baseline → PodDisruptionBudget → ArgoCD Application), spec validation
with status reporting, and fake-client tests at 90% controller coverage.

## M1 — correctness hardening ✅ (v0.2.0)

The reconciler creates objects; it doesn't yet own their whole lifecycle.

- [x] Finalizer (`idp.jiung9.github.io/cleanup`) + garbage collection of everything
      provisioned. Deletion order: ArgoCD Application first, then the namespace (which
      cascades to the ConfigMap/PDB inside it). Every delete is idempotent — partial
      provisioning is safe to unwind because object names are derived, never stored.
      Why a finalizer and not ownerReferences: the namespace is cluster-scoped and the
      Application lives in `argocd` — both outside the CR's namespace, where ownerRefs
      are illegal.
- [x] PDB derived from `spec.replicas` instead of a hardcoded `minAvailable: 1`:
      replicas ≥ 2 → `maxUnavailable: 1` · replicas ≤ 1 → **no PDB**, plus a status
      condition saying why. A single-replica service can't be disruption-safe;
      `minAvailable: 1` would just block node drains without protecting anything. The
      condition makes "intentionally unprotected" distinguishable from "misconfigured".
- [x] Conditions managed with `meta.SetStatusCondition` (real transition times, per-type
      history) — lands before any later milestone adds more condition types.
- [x] Defaults move into the CRD schema (`+kubebuilder:default`); in-controller
      defaulting stays as idempotent defense-in-depth.
- [x] Drift detection: label-mapped `Watches()` on provisioned objects (ownerRefs can't
      cover cluster-scoped or cross-namespace children, so drift is re-queued by label).
- [x] RBAC markers + generated role; installable `config/` manifests.
- [x] `controller-gen`: generated deepcopy + CRD manifests replace the hand-written ones.
- [x] envtest suite alongside the fake-client tests — reconcile against real API-server
      semantics (conflicts, resourceVersion, deletion timestamps).

**Exit:** create → verify → delete round-trip green in envtest (including deletion
mid-provision); `go vet` clean; generated output committed and in sync. **Met.**

Two guards were added that the original list didn't anticipate, both from reconciling
against a real API server: the namespace-ownership check (refuse to adopt a namespace
this operator didn't create, or one owned by another `ServiceDeployment`) and reading
that check through an uncached reader, because the manager cache is label-filtered and
would otherwise hide a foreign namespace — during teardown that gap would have released
the finalizer without deleting anything.

## M2 — policy baseline ✅ (v0.3.0)

Provisioning should hand out guardrails, not just resources.

- [x] NetworkPolicy default-deny **ingress** per provisioned namespace, re-admitting
      the namespace itself and the platform's declared ingress sources
      (`-allow-ingress-from-namespaces`, `-allow-ingress-from-cidrs`).
      **Egress is deliberately untouched** — see the scope note below.
- [x] Kyverno policy baseline: privileged pods and host namespaces rejected, plus
      Cosign signature verification when a signing identity is configured. Applied as
      unstructured objects, so no Kyverno Go dependency. If Kyverno isn't installed,
      `AdmissionPolicyReady=False/KyvernoNotInstalled` and everything else still
      provisions — degrade, don't crash — and the reconcile requeues, because nothing
      in the watch set fires when a CRD is installed later.
- [x] Status conditions per provisioned component (`NamespaceReady`, `SLOReady`,
      `PDBReady`, `NetworkPolicyReady`, `AdmissionPolicyReady`, `DeliveryReady`).
      Component conditions are independent and all surfaced; the aggregate
      `Provisioned` condition distinguishes a **deliberate skip** from a **failure**.
      A strict AND would have reported single-replica services as unprovisioned,
      contradicting M1: a skip is a documented state, and the aggregate's reason names
      every one of them so "degraded" is never silent.

**Exit:** a fresh `ServiceDeployment` yields a namespace carrying the deny-by-default
ingress policies and a Kyverno baseline whose rules say what they enforce, with
`.status.conditions` explaining anything absent. **Met.**

**What this milestone does NOT prove:** envtest runs an API server with no admission
webhooks, so it verifies the policies are provisioned and correct, not that they
*reject* anything. Actual rejection of a privileged or unsigned pod is proven by the
kind-based e2e in M4. Claiming otherwise on envtest evidence would be a lie.

### Scope note: why ingress only

An egress default-deny is only useful once a service can declare what it talks to.
Without that declaration it is an outage with a policy attached, and it cannot be
demonstrated in envtest either — proving it needs a real workload reaching a real
dependency, which is M4's territory. The declaration (`spec.network.allowEgressTo`,
turning declared dependencies into policy) is the natural follow-up and belongs in
its own milestone, not smuggled into this one untested.

## M3 — budget-aware delivery (the point of the project)

Enforce the SLO baseline via the **allow / require-human / block** verdict contract
that [error-budget-gate](https://github.com/JIUNG9/error-budget-gate) defines. That
project is Python; this operator implements the same contract natively in Go — the two
share semantics, not code.

- [ ] `spec.tier` (0–3) and `spec.environment` join the API. The gated action is fixed
      by construction: automated ArgoCD sync to a declared Git revision — reversible,
      because revert is a sync to the previous revision.
- [ ] `internal/budget`: verdict(tier, env, budget_remaining) with error-budget-gate's
      thresholds (block on exhausted budget or tier-0 prod; require-human on prod or
      <25% remaining; allow otherwise), unit-tested against its documented cases.
- [ ] `BurnSource` interface returning normalized `budget_remaining` ∈ [0,1]
      (1 − observed error rate ÷ allowed error rate, over the SLO window):
      a Prometheus implementation (documented query shape) + a static implementation
      for tests and the demo.
- [ ] The verdict is persisted state, not an in-memory opinion — `status.budget` holds
      verdict, budgetRemaining, streak, lastEvaluationTime. Deterministic sync-policy
      mapping: allow → automated sync · require-human → manual sync + condition ·
      block → automated sync removed + condition. Tightening applies immediately
      (fail closed); loosening needs N consecutive evaluations, and the streak survives
      operator restarts because it lives in status.
- [ ] Tests for all three verdicts, verdict flapping, and restart survival.

**Exit:** envtest demo — a budget-exhausted service loses automated sync, the
`ServiceDeployment` status explains it, and the verdict survives an operator restart.

## M4 — ship

- [ ] kind-based e2e: the README quickstart executed verbatim by a script, asserting
      the full golden path — including a correctly-labeled workload proving the label
      contract below (used as CI's e2e job and as the demo)
- [ ] CI workflow: build, vet, test, generated-output-in-sync check (+ e2e on label),
      coverage badge
- [ ] README quickstart verified verbatim on a clean machine; demo GIF
- [ ] Example gallery: minimal, full-spec, and budget-gated `ServiceDeployment`s
- [ ] Final review of history and tree for anything non-public, then flip to public

**Exit:** public repo, CI green on the first public commit.

## The label contract

The PDB (and everything else the operator provisions for a service) selects workloads
by `app.kubernetes.io/name: <service-name>`. The operator never creates the workload —
your GitOps repo does — so your manifests must carry that label. A mislabeled workload
means a PDB that silently selects nothing. The e2e proves the contract end to end;
enforcement (a condition verifying the label exists on live workloads) is future work.

## Next after M4

- **`spec.network.allowEgressTo`** — declared dependencies become egress policy, which
  is what makes an egress default-deny safe to ship. Split out of M2 on purpose.
- **Live workload verification** — a condition that checks the `app.kubernetes.io/name`
  label actually exists on running workloads, so a mislabeled repo fails loudly instead
  of leaving a PDB that selects nothing.

## Non-goals (for now)

Multi-cluster placement, a UI, Backstage plugin, template scaffolding of the app repo
itself, and reacting to live workload changes (HPA-driven replica counts — v1 derives
disruption policy from the declared spec only). The scope is the reliability golden
path: provisioning with SLOs, disruption budgets, policy, and budget-gated delivery
as defaults.
