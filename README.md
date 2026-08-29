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
- an **ArgoCD Application** — delivery is Git-to-production from the first commit

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

Every component reports its own status condition — `NamespaceReady`, `SLOReady`,
`PDBReady`, `NetworkPolicyReady`, `AdmissionPolicyReady`, `DeliveryReady` — and the
aggregate `Provisioned` condition names anything that was deliberately skipped. A
guardrail that isn't there says so; it never reads as healthy.

## Status: early development

This is a young project and says so. What works today:

- `ServiceDeployment` API types + CRD, with schema defaulting and validation
- Reconciler provisioning namespace → SLO baseline → PDB → NetworkPolicies →
  Kyverno baseline → ArgoCD Application, with finalizer-based cleanup
- Spec validation with clear rejection reasons (surfaced in `.status`)
- Unit, fake-client, and envtest suites (`make test`)

## Why SLO-first

Most IDPs treat reliability as something teams add later. This one bakes the SLO
into provisioning: every service gets a target the moment it exists, and the
baseline ConfigMap is a contract that alerting and error-budget tooling
(see [error-budget-gate](https://github.com/JIUNG9/error-budget-gate)) can read.
The golden path is a reliability path.

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
- **Objects named `idp-*` in a provisioned namespace are the operator's.** It
  will take over and overwrite one that already exists, including one the cache
  could not see. That is deliberate for guardrails, and worth knowing before you
  name something `idp-` yourself.

## The label contract

The PDB selects workloads by `app.kubernetes.io/name: <service-name>`. The operator
never creates your workload — your GitOps repo does — so your manifests must carry
that label, or the PDB silently selects nothing.

## Roadmap

See [ROADMAP.md](ROADMAP.md). Next up: budget-aware sync gating, implementing the
allow / require-human / block verdict contract defined by
[error-budget-gate](https://github.com/JIUNG9/error-budget-gate) natively in Go —
shared semantics, not shared code.

## Development

```
make tools     # controller-gen + setup-envtest
make test      # vet + unit + envtest
make manifests # regenerate CRD and RBAC from markers
```

## License

MIT
