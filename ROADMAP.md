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

## M3 — budget-aware delivery ✅ (v0.4.0)

The SLO baseline is enforced, not just declared: a service's remaining error budget
decides whether ArgoCD may sync it unattended.

- [x] `spec.tier` (0–3) and `spec.environment` join the API. The gated action is fixed
      by construction: automated sync to a declared Git revision — reversible, because
      revert is a sync to the previous revision.
- [x] `internal/budget`: verdict(tier, environment, remaining) sharing
      [error-budget-gate](https://github.com/JIUNG9/error-budget-gate)'s vocabulary and
      thresholds, with **one deliberate divergence** — production alone does not require
      a human here, because the gated action is a human's own reviewed commit rather
      than a remediation a robot invented. The full comparison is in the README and on
      `Evaluate`.
- [x] `BurnSource` returning normalized remaining budget ∈ [0,1]: a Prometheus
      implementation over a configurable query template, and a static one for demos.
      A source that errors, returns no series, or returns NaN is **no evidence** — never
      a healthy budget.
- [x] The verdict is persisted state in `status.budget` (verdict, reading, streak,
      source, timestamp), so it survives operator restarts. Tightening applies
      immediately; relaxing waits for `LooseningStreak` agreeing readings.
- [x] Tests for all three verdicts, flapping, restart survival, and failing closed.

**Exit:** envtest — an exhausted budget takes automated sync off the real Application
object, `.status` explains it, a restarted operator does not restore it on the first
healthy reading, and it converges back to automated once the budget genuinely recovers.
**Met.**

### The bug envtest found that the unit tests could not

The anti-flapping rule first counted **reconciles**. Writing status triggers a watch
event, which triggers another reconcile, so the streak burned from 0 to 3 in
milliseconds and the gate reopened almost the instant a budget blipped upward — the
exact behaviour the rule exists to prevent. Unit tests could never show it: they call
`Reconcile` by hand, so the loop never runs. Re-evaluation is now paced by elapsed
time, which also stops the operator querying Prometheus once per event.

## M4 — ship ✅ (v0.5.0)

- [x] Dockerfile: distroless, non-root, static binary
- [x] kind-based e2e (`test/e2e/e2e.sh`) — the README quickstart executed, **run green
      2026-09-01**. It is the only place that proves what envtest cannot: the Kyverno
      baseline actually **rejects** a privileged pod, and the PDB really selects a
      correctly labelled workload. It also asserts an ordinary pod is still admitted,
      because a baseline that fails every normal manifest gets switched off.
- [x] CI: build, vet, gofmt, generated-output-in-sync, unit + envtest; e2e on demand
      (PR label `e2e` or manual dispatch, since it builds an image and stands up a
      cluster)
- [x] OSS hygiene workflow + `docs/OSS_HYGIENE.md`, from the shared template
- [x] Example gallery: minimal, full-spec, budget-gated
- [ ] Demo GIF
- [ ] Final review of history and tree for anything non-public, then flip to public

**Exit:** public repo, CI green on the first public commit. **Half met.** The engineering
gate — the e2e — is green. What remains is a push-time event: no local action can make CI
run on a public commit, so that box stays open until the repo is public.

### What the first e2e run found

Both findings were in the test, not the operator — established by inspecting the live
cluster rather than assuming, which is the only reason that claim is worth anything.

1. **Kyverno cannot be installed with a client-side apply.** Its CRDs carry enormous
   descriptions, and client-side apply stores the entire manifest in the
   `last-applied-configuration` annotation, which the API server caps at 262144 bytes.
   It fails on `clusterpolicies` and `policies`. Server-side apply does not write that
   annotation.
2. **The label-contract assertion read the wrong field.** `expectedPods` was 0, which
   looks exactly like a broken selector. It is not: that field derives from the scale
   subresource of whatever controls the pods, and the probe is a bare Pod with no
   Deployment behind it. `currentHealthy` — the count of healthy pods the selector
   actually matched — was 1 throughout.

## The label contract

The PDB (and everything else the operator provisions for a service) selects workloads
by `app.kubernetes.io/name: <service-name>`. The operator never creates the workload —
your GitOps repo does — so your manifests must carry that label. A mislabeled workload
means a PDB that silently selects nothing. The e2e proves the contract end to end;
the `WorkloadsSelected` condition reports any Deployment, StatefulSet or Argo Rollout whose pod template lacks it.
It checks pod templates (and the labels of bare Pods), not the running pods a controller creates, and
reads nothing but Deployments, StatefulSets, Rollouts and Pods.

## Next after M4

- **`spec.network.allowEgressTo`** — declared dependencies become egress policy, which
  is what makes an egress default-deny safe to ship. Split out of M2 on purpose.
- **Live workload verification, remaining part** — `WorkloadsSelected` covers Deployments and
  StatefulSets and Argo Rollouts by pod template, and bare Pods by their own labels. The running pods a
  controller creates are not checked.
- **Platform-enforced environment** — `spec.environment` weakens the budget verdict and
  is self-service, so a team can relabel prod as dev to win back automated delivery
  below the 25% line. Exhaustion still blocks everywhere and the gate records what it
  believed, but an operator-side mapping (namespace or label to environment) would make
  it enforcement rather than governance.
- **Kyverno enforcement readback** — read the provisioned policy's own status back, so
  `AdmissionPolicyReady` reflects whether Kyverno accepted and is enforcing it, not just
  that the API server stored it. Today a broken Kyverno reads as green.

## Non-goals (for now)

Multi-cluster placement, a UI, Backstage plugin, template scaffolding of the app repo
itself, and reacting to live workload changes (HPA-driven replica counts — v1 derives
disruption policy from the declared spec only). The scope is the reliability golden
path: provisioning with SLOs, disruption budgets, policy, and budget-gated delivery
as defaults.
