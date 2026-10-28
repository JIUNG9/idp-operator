# idp-operator

A Kubernetes operator for golden-path service provisioning. One `ServiceDeployment`
manifest gets a team everything a production service needs on day one:

- a **namespace**, labeled with ownership
- an **SLO baseline** — a machine-readable reliability contract (`99.9%` availability,
  `300ms` p99 by default; silence is not an opt-out)
- a **PodDisruptionBudget** derived from the declared replicas, so disruption safety
  is the default, not a request (single-replica services get an honest status condition
  instead of a drain-blocking fake PDB)
- **NetworkPolicies** that deny ingress by default and re-admit only the namespace
  itself and the platform's declared ingress path
- a **Kyverno baseline** rejecting privileged pods and host namespaces, and verifying
  image signatures when the platform has a signing identity configured
- an **ArgoCD Application** — delivery is Git-to-production from the first commit,
  for exactly as long as the service's error budget can afford it

```yaml
apiVersion: idp.jiung9.github.io/v1alpha1
kind: ServiceDeployment
metadata:
  name: orders-api
  namespace: idp-system
spec:
  team: checkout
  gitops:
    repoURL: https://github.com/acme-corp/orders-api-deploy
    path: overlays/prod
```

That's the whole onboarding. See `examples/orders-api.yaml` for the full shape.

Teams that use Backstage can fill in a form instead: see [Backstage template](#backstage-template).

Every component reports its own status condition — `NamespaceReady`, `SLOReady`,
`PDBReady`, `NetworkPolicyReady`, `AdmissionPolicyReady`, `BudgetGateReady`,
`DeliveryReady`, `WorkloadsSelected` — and the aggregate `Provisioned` condition names anything that was
skipped or is degraded. A guardrail that isn't there says so; it never reads as
healthy.

`WorkloadsSelected` reads the Deployments, StatefulSets and bare Pods in the service namespace and
reports any whose pod template (or, for a Pod, labels) lacks `app.kubernetes.io/name: <service>`, the label the PDB
selects on. It is absent for services without a PDB, and reads `NoWorkloads` until the first
workload is delivered. Bare Pods (no controller owner) are checked on their own labels. It never
edits a workload.

## Status: early development

This is a young project and says so. What works today:

- `ServiceDeployment` API types + CRD, with schema defaulting and validation
- Reconciler provisioning namespace → SLO baseline → PDB → NetworkPolicies →
  Kyverno baseline → budget-gated ArgoCD Application, with finalizer-based cleanup
- Error-budget gating of automated delivery, with the verdict persisted so it
  survives restarts
- Spec validation with clear rejection reasons (surfaced in `.status`)
- Unit, fake-client, and envtest suites (`make test`)
- A kind-based end-to-end test (`./test/e2e/e2e.sh`) that runs the quickstart on a real
  cluster and proves the two things envtest structurally cannot: that the Kyverno
  baseline **rejects** a privileged pod rather than merely existing, and that the
  PodDisruptionBudget really selects a correctly labelled workload

## Why SLO-first

Most IDPs treat reliability as something teams add later. This one bakes the SLO
into provisioning: every service gets a target the moment it exists, and that
target is not decoration — it decides whether the service is still allowed to
deploy itself. The golden path is a reliability path.

## Budget-aware delivery

Point the operator at Prometheus and every service's ArgoCD sync policy becomes a
function of its remaining error budget:

| Verdict | What happens |
| --- | --- |
| `allow` | Automated sync stays on. Push to Git, it ships. |
| `require-human` | Automated sync is removed; someone syncs deliberately. |
| `block` | Same mechanism — ArgoCD has no stronger stop — but the status says this is an emergency, not a speed bump. |

The verdict lives in `status.budget` alongside the reading it came from, so
`kubectl get servicedeployment` shows you which services have deployed themselves
into a corner.

**Tightening is immediate. Relaxing waits for three agreeing readings**, paced by
`-budget-reevaluation-interval` rather than by reconcile count — the operator's own
status writes trigger reconciles, so a reconcile-counted streak would burn through
in milliseconds and reopen the gate the instant a budget blipped upward.

**A burn source that cannot be read is not a healthy budget.** If Prometheus is
down or the query returns nothing, the operator holds the last verdict it actually
measured — for a while. A Prometheus restart is not a reliability event, and turning
every scrape blip into a delivery freeze teaches people to rip the gate out. But the
hold is bounded at three re-evaluation intervals, because monitoring tends to fail
alongside the things it monitors: an unbounded hold would keep delivery unattended
through exactly the incident nobody can see. Past that, an unreadable budget tightens
the gate to `require-human`. It never loosens one — a `block` that cannot be
re-measured stays a `block`.

### How this relates to error-budget-gate

This shares [error-budget-gate](https://github.com/JIUNG9/error-budget-gate)'s
verdict vocabulary and its budget thresholds. It is a native Go implementation of
the same idea, not a port of the Python — and one rule differs on purpose:

| Situation | error-budget-gate | this operator |
| --- | --- | --- |
| Budget exhausted | block | block |
| Tier-0 in production | block | block once the budget is low |
| Production, healthy budget | require-human | **allow** |
| Production, budget under 25% | require-human | require-human |
| Non-production, budget under 25% | require-human | **allow** |

error-budget-gate asks for a human on any production action because the action it
gates is one an automated remediation system invented by itself. What gets gated
here is delivery of a commit a human already wrote, reviewed and merged. Copying
the rule verbatim would take automated sync away from every production service
forever — not a safety property, just the headline feature switched off in the only
environment anyone cares about. So production alone allows here; a depleted budget
is what stops it. And a dev service with a low budget keeps shipping, because
stopping it is friction with nothing on the other side.

## Platform configuration

Two things belong to the cluster rather than to any one service, so they are
operator flags, not spec fields:

| Flag | What it does |
| --- | --- |
| `-allow-ingress-from-namespaces` | Namespaces allowed to reach every service — typically your ingress controller's. **Without this, nothing outside a service's own namespace can reach it.** |
| `-allow-ingress-from-cidrs` | Source ranges for load balancers that target pod IPs directly and have no namespace to select (AWS Load Balancer Controller with `target-type=ip`, for one) |
| `-cosign-key-file` | PEM public key, usually a mounted Secret, that images must be signed with |
| `-cosign-issuer` / `-cosign-subject` | Keyless (Fulcio) verification instead of a key |
| `-verify-image-references` | Image globs verification applies to (default `*`) |
| `-require-image-digests` | Reject images referenced by tag. Off by default; see below |
| `-prometheus-url` | Where to read error budgets from. **Empty means delivery is not budget-gated at all** |
| `-burn-query` | Go template for the instant query returning a service's error ratio (`{{.Namespace}}`, `{{.Name}}`, `{{.Window}}`) |
| `-budget-reevaluation-interval` | How stale a reading may get, and the pace of the anti-flapping rule (default 5m) |
| `-static-budget` | A fixed budget fraction instead of Prometheus. Demos and testing only |

Without a signing identity the workload rules still ship and the status condition
says plainly that signatures are **not** verified.

### Two decisions worth knowing about

**Image verification never rewrites the image.** Kyverno's `verifyImages` defaults
to `mutateDigest: true`, replacing the tag with the verified digest at admission.
Every service here also gets an ArgoCD Application with `selfHeal` on, so that
rewrite becomes a standing fight: Kyverno mutates, ArgoCD sees drift from Git and
puts the tag back, Kyverno mutates again. This operator verifies the signature and
leaves the reference alone.

That has a cost, and it should be stated plainly: verifying a tag is a check with
a shelf life. The tag can be moved to an unsigned image after admission, or before
the kubelet pulls it, and what runs is not what was verified. Closing that gap
means the workload must name a digest — which is why `-require-image-digests`
exists, and why the digest has to come from the pipeline that builds your
manifests rather than from admission. It is off by default because turning it on
rejects every tag-based manifest in the cluster.

**Ingress is denied by default; egress is not.** An egress default-deny is only
useful once a service can declare what it talks to, and that declaration doesn't
exist yet — shipping one now would be an outage with a policy attached. So be
clear about what this baseline does and doesn't do: it limits who can reach a
service, not what a compromised pod inside it can reach. Containment is
`allowEgressTo` on the roadmap, not something you have today.

## What this operator does not check

- **Kyverno's health.** The operator confirms the cluster knows what a Kyverno
  Policy is, writes one, and reports that it was accepted. Whether it is
  *enforced* depends on Kyverno's admission webhook being alive, which is not
  verified here — a broken Kyverno admits privileged pods while
  `AdmissionPolicyReady` still reads true. The condition message says so.
- **`spec.environment` is a declaration, not a security boundary.** A team that can
  edit its own `ServiceDeployment` can relabel a production service as `dev` and win
  back automated delivery in the 0–25% band. Two things bound it: an *exhausted*
  budget blocks in every environment, and the gate records the environment and tier
  it believed into `status.budget`, so the relabel is visible in the object's own
  status rather than being invisible. Platform-enforced environments are on the
  roadmap; today this is governance, not enforcement.
- **Whether your metrics mean what the burn query assumes.** The default query
  counts `http_requests_total` with a `5..` status. If your services do not emit
  that, replace it with `-burn-query`; a query that matches nothing is treated as
  no evidence and fails closed, which is safe but not useful.
- **Objects named `idp-*` in a provisioned namespace are the operator's.** It
  will take over and overwrite one that already exists, including one the cache
  could not see. That is deliberate for guardrails, and worth knowing before you
  name something `idp-` yourself.

## The label contract

The PDB selects workloads by `app.kubernetes.io/name: <service-name>`. The operator
never creates your workload — your GitOps repo does — so your manifests must carry
that label, or the PDB silently selects nothing.

## Backstage template

`templates/backstage/service-deployment/` is a software template (`scaffolder.backstage.io/v1beta3`).
The form collects the `ServiceDeployment` fields, renders `skeleton/` into `services/<name>.yaml`, and opens a
pull request with `publish:github:pull-request`. The operator picks the manifest up after it merges, so the
form never talks to the cluster.

To use it, register `template.yaml` in your Backstage catalog and change `owner` and `repo` in the
`pull-request` step to the repository your platform manifests are applied from. The skeleton writes
`namespace: idp-system`, the namespace used in `examples/`; edit it if the operator watches another one.

The form is a second copy of the CRD schema, so `go test ./test/scaffolder` checks it: every spec field has a form
field with the same type, default, enum, bounds and pattern, the CRD's required fields are required in the form,
and the skeleton renders to a manifest that decodes into the API type. Changing the CRD without the form fails
that test.

## Roadmap

See [ROADMAP.md](ROADMAP.md). Next up: `spec.network.allowEgressTo`, which is what makes
an egress default-deny safe to ship, and reading Kyverno's own policy status back so
`AdmissionPolicyReady` reflects enforcement rather than storage.

## Development

```
make tools     # controller-gen + setup-envtest
make test      # vet + unit + envtest
make manifests # regenerate CRD and RBAC from markers
```

The end-to-end test needs Docker and kind. It creates a throwaway cluster and
deletes it on exit:

```
./test/e2e/e2e.sh          # KEEP=1 to leave the cluster up for inspection
```

## License

MIT
