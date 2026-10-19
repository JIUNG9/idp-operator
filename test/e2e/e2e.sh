#!/usr/bin/env bash
#
# End-to-end test on a throwaway kind cluster.
#
# This is the README quickstart, executed. It is also the only place that
# proves two things envtest cannot:
#
#   1. The Kyverno baseline actually REJECTS a privileged pod. envtest runs an
#      API server with no admission webhooks, so there it can only be shown to
#      exist.
#   2. The label contract holds end to end — a workload carrying
#      app.kubernetes.io/name is genuinely selected by the PodDisruptionBudget
#      the operator provisioned for it.
#
# Everything it creates lives in one kind cluster that is deleted on exit.
set -euo pipefail

CLUSTER="${CLUSTER:-idp-e2e}"
KUBECTL="kubectl --context kind-${CLUSTER}"
KEEP="${KEEP:-0}"
PROBE_IMAGE="${PROBE_IMAGE:-registry.k8s.io/pause:3.10}"
KYVERNO_VERSION="${KYVERNO_VERSION:-v1.13.4}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
ok()   { printf '\033[1;32m  ok: %s\033[0m\n' "$*"; }

# retry_until <seconds> <expected> <command...> — poll until the command's
# stdout equals <expected>. Several of the things asserted below are written by
# other controllers after the fact, so a single read races them.
retry_until() {
  local timeout=$1 expected=$2; shift 2
  local deadline=$(( SECONDS + timeout )) actual=""
  while [ $SECONDS -lt $deadline ]; do
    actual=$("$@" 2>/dev/null || true)
    [ "$actual" = "$expected" ] && return 0
    sleep 2
  done
  printf 'last value: %s\n' "${actual:-<empty>}" >&2
  return 1
}

# diagnose — on failure, print what an engineer would otherwise have to
# reproduce locally: why pods are not Ready (events, image pulls, scheduling)
# and what the operator itself said. A bare "timed out waiting for the
# condition" on a CI runner tells you nothing else.
diagnose() {
  log "diagnostics (the run failed)"
  {
    echo "--- pods, all namespaces"
    $KUBECTL get pods -A -o wide
    echo "--- describe pods in orders-api"
    $KUBECTL describe pods -n orders-api
    echo "--- events in orders-api"
    $KUBECTL get events -n orders-api --sort-by=.lastTimestamp
    echo "--- operator logs (last 200 lines)"
    $KUBECTL -n idp-system logs deploy/idp-operator --tail=200
    echo "--- servicedeployment status"
    $KUBECTL get servicedeployment -n idp-system -o yaml
  } 2>&1 || true
}

cleanup() {
  local code=$?
  # The cluster is still up here, so this is the last chance to look at it.
  if [ "$code" -ne 0 ] && kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    diagnose
  fi
  if [ "$KEEP" = "1" ]; then
    echo "KEEP=1, leaving cluster ${CLUSTER} up for inspection"
  else
    log "tearing down"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  exit $code
}
trap cleanup EXIT

for bin in kind kubectl docker go; do
  command -v "$bin" >/dev/null || fail "$bin is required"
done

log "creating kind cluster ${CLUSTER}"
kind create cluster --name "$CLUSTER" --wait 120s

log "installing Kyverno ${KYVERNO_VERSION}"
# Server-side apply is required, not a preference. Kyverno's CRDs carry huge
# descriptions, and a client-side apply stores the entire manifest in the
# kubectl.kubernetes.io/last-applied-configuration annotation, which the API
# server caps at 262144 bytes. Client-side fails with "metadata.annotations:
# Too long" on clusterpolicies and policies.
$KUBECTL apply --server-side --force-conflicts \
  -f "https://github.com/kyverno/kyverno/releases/download/${KYVERNO_VERSION}/install.yaml"
$KUBECTL -n kyverno rollout status deploy/kyverno-admission-controller --timeout=300s

# Only ArgoCD's CRD, not ArgoCD itself: this test asserts that the operator
# writes a correct Application object, not that ArgoCD then syncs it. Running
# a whole GitOps controller would test ArgoCD, not this.
log "installing the ArgoCD Application CRD"
$KUBECTL apply -f "$REPO_ROOT/test/crds/argoproj.io_applications.yaml"
$KUBECTL create namespace argocd

log "building and loading the operator image"
docker build -t idp-operator:e2e "$REPO_ROOT"
kind load docker-image idp-operator:e2e --name "$CLUSTER"

log "installing the operator"
$KUBECTL apply -f "$REPO_ROOT/config/crd/"
$KUBECTL apply -f "$REPO_ROOT/config/manager/manager.yaml"
$KUBECTL apply -f "$REPO_ROOT/config/rbac/"
$KUBECTL -n idp-system set image deploy/idp-operator manager=idp-operator:e2e
$KUBECTL -n idp-system patch deploy/idp-operator --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"}]'
$KUBECTL -n idp-system rollout status deploy/idp-operator --timeout=180s

# The probe pods run the pause image, which every kind node already carries as
# its sandbox container, so nothing is pulled. The first version pulled busybox
# from a public registry and timed out on shared CI runners with
# ErrImagePull / 429 Too Many Requests: the test depended on a service it
# does not care about. Override with PROBE_IMAGE if your node image differs.
log "checking ${PROBE_IMAGE} is on the node"
docker exec "${CLUSTER}-control-plane" crictl images 2>/dev/null | grep -q "${PROBE_IMAGE%%:*}" \
  || fail "${PROBE_IMAGE} is not on the kind node; set PROBE_IMAGE to an image it carries"

log "the quickstart: one manifest"
$KUBECTL apply -f "$REPO_ROOT/examples/minimal.yaml"

log "waiting for the golden path"
$KUBECTL wait --for=jsonpath='{.status.phase}'=Provisioned \
  servicedeployment/orders-api -n idp-system --timeout=120s

# --- what the manifest bought -------------------------------------------------
log "checking what one manifest provisioned"

$KUBECTL get namespace orders-api >/dev/null || fail "namespace not created"
ok "namespace"

$KUBECTL get configmap slo-baseline -n orders-api >/dev/null || fail "SLO baseline not created"
target=$($KUBECTL get configmap slo-baseline -n orders-api -o jsonpath='{.data.availability_target}')
[ "$target" = "99.9" ] || fail "SLO default not applied, got '${target}'"
ok "SLO baseline defaulted to ${target}%"

for np in idp-default-deny-ingress idp-allow-same-namespace; do
  $KUBECTL get networkpolicy "$np" -n orders-api >/dev/null || fail "$np not created"
done
ok "network policies"

$KUBECTL get policy idp-baseline -n orders-api >/dev/null || fail "kyverno policy not created"
ok "kyverno baseline"

$KUBECTL get application orders-api -n argocd >/dev/null || fail "argocd application not created"
ok "argocd application"

# The minimal example declares no replicas, so it defaults to 1 and gets no
# PDB — a single replica cannot be disruption-safe, and the status says so
# rather than shipping a PDB that only blocks drains.
if $KUBECTL get poddisruptionbudget orders-api -n orders-api >/dev/null 2>&1; then
  fail "a single-replica service must not get a PDB"
fi
reason=$($KUBECTL get servicedeployment orders-api -n idp-system \
  -o jsonpath='{.status.conditions[?(@.type=="PDBReady")].reason}')
[ "$reason" = "SingleReplicaUnprotected" ] || fail "expected SingleReplicaUnprotected, got '${reason}'"
ok "no PDB at one replica, and the status explains it"

# --- the guardrail actually guards -------------------------------------------
log "checking the Kyverno baseline rejects a privileged pod"
if $KUBECTL apply -f - <<EOF 2>/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: privileged-probe
  namespace: orders-api
spec:
  containers:
    - name: probe
      image: ${PROBE_IMAGE}
      securityContext:
        privileged: true
EOF
then
  fail "a privileged pod was admitted; the baseline is not being enforced"
fi
ok "privileged pod rejected at admission"

# A pod that asks for nothing unusual still goes through — a baseline that
# fails every ordinary manifest would just get switched off. It carries the
# label the golden path selects on, which the PDB check below depends on.
$KUBECTL apply -f - <<EOF || fail "an ordinary pod was rejected"
apiVersion: v1
kind: Pod
metadata:
  name: ordinary-probe
  namespace: orders-api
  labels:
    app.kubernetes.io/name: orders-api
spec:
  containers:
    - name: probe
      image: ${PROBE_IMAGE}
EOF
ok "ordinary pod admitted"

# --- the label contract -------------------------------------------------------
log "checking the PDB label contract end to end"
$KUBECTL patch servicedeployment orders-api -n idp-system --type=merge -p '{"spec":{"replicas":2}}'
$KUBECTL wait --for=jsonpath='{.status.conditions[?(@.type=="PDBReady")].status}'=True \
  servicedeployment/orders-api -n idp-system --timeout=60s

$KUBECTL wait --for=condition=Ready pod/ordinary-probe -n orders-api --timeout=120s
# The workload carries app.kubernetes.io/name, so the PDB the operator wrote
# must actually select it. A mislabelled workload would leave the PDB
# protecting nothing, silently — which is the whole reason the contract is
# documented in the README.
# currentHealthy is the field that answers "did the selector match?" — it is
# the count of healthy pods the PDB selected, written by the disruption
# controller once it has counted them.
#
# NOT expectedPods, which is 0 here and looks like a broken selector but is
# not: expectedPods comes from the scale subresource of whatever controls the
# pods, and this probe is a bare Pod with no Deployment behind it. A bare pod
# is deliberate — it isolates the label contract from any question about
# replica-set behaviour, which is Kubernetes' business rather than this
# operator's.
retry_until 60 "1" $KUBECTL get poddisruptionbudget orders-api -n orders-api \
  -o jsonpath='{.status.currentHealthy}' \
  || fail "PDB selected no pods; the label contract is broken"
ok "PDB selects the labelled workload"

log "all e2e checks passed"
