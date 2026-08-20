# idp-operator

A Kubernetes operator for golden-path service provisioning. One `ServiceDeployment`
manifest gets a team everything a production service needs on day one:

- a **namespace**, labeled with ownership
- an **SLO baseline** — a machine-readable reliability contract (`99.9%` availability,
  `300ms` p99 by default; silence is not an opt-out)
- a **PodDisruptionBudget**, so disruption safety is the default, not a request
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

## Status: early development

This is a young project and says so. What works today:

- `ServiceDeployment` API types + CRD
- Reconciler provisioning namespace → SLO baseline → PDB → ArgoCD Application
- Spec validation with clear rejection reasons (surfaced in `.status`)
- Unit + fake-client reconcile tests (`go test ./...`)

## Why SLO-first

Most IDPs treat reliability as something teams add later. This one bakes the SLO
into provisioning: every service gets a target the moment it exists, and the
baseline ConfigMap is a contract that alerting and error-budget tooling
(see [error-budget-gate](https://github.com/JIUNG9/error-budget-gate)) can read.
The golden path is a reliability path.

## Roadmap

- [ ] Finalizers + garbage collection of provisioned objects
- [ ] `error-budget-gate` integration: budget-aware sync gating
- [ ] Kyverno policy baseline (signed images / Cosign verification)
- [ ] NetworkPolicy default-deny baseline
- [ ] Generated deepcopy + CRD via controller-gen
- [ ] E2E against kind

## Development

```
go build ./...
go test ./...
```

## License

MIT
