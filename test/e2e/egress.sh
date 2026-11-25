#!/usr/bin/env bash
#
# Egress enforcement on a kind cluster whose CNI enforces NetworkPolicy.
#
# envtest has no CNI, and kind's default CNI (kindnet) does not enforce
# NetworkPolicy, so neither can show a blocked connection. This script installs
# Calico, starts the operator with -enforce-egress, and asserts, with real
# TCP connections:
#
#   1. a pod in a service that declared allowEgressTo: [server] reaches a pod
#      in `server`;
#   2. the same pod cannot reach a pod in `other`, which it did not declare;
#   3. control: a pod outside any egress policy (namespace `default`) does
#      reach the pod in `other`, so the block in (2) comes from the egress
#      policy and not from the destination's ingress rules.
#
# Everything it creates lives in one kind cluster that is deleted on exit.
set -euo pipefail

CLUSTER="${CLUSTER:-idp-egress}"
KUBECTL="kubectl --context kind-${CLUSTER}"
KEEP="${KEEP:-0}"
CALICO_VERSION="${CALICO_VERSION:-v3.29.1}"
# agnhost serves HTTP (netexec) and opens TCP connections (connect), so one
# image is both the destination and the probe. The nodes pull it from
# registry.k8s.io, which does not rate-limit CI runners the way Docker Hub
# does. It is not loaded with `kind load`: that fails on this multi-platform
# image ("content digest ... not found").
AGNHOST_IMAGE="${AGNHOST_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.53}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
ok()   { printf '\033[1;32m  ok: %s\033[0m\n' "$*"; }

diagnose() {
  log "diagnostics (the run failed)"
  {
    echo "--- pods, all namespaces"
    $KUBECTL get pods -A -o wide
    echo "--- network policies"
    $KUBECTL get networkpolicy -A
    echo "--- operator logs (last 100 lines)"
    $KUBECTL -n idp-system logs deploy/idp-operator --tail=100
    echo "--- servicedeployments"
    $KUBECTL get servicedeployment -n idp-system -o yaml
  } 2>&1 || true
}

cleanup() {
  local code=$?
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

log "creating kind cluster ${CLUSTER} without the default CNI"
# kindnet does not enforce NetworkPolicy. Calico's default pod CIDR is
# 192.168.0.0/16, which the cluster is told to use.
kind create cluster --name "$CLUSTER" --wait 0s --config - <<CFG
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 192.168.0.0/16
CFG

log "installing Calico ${CALICO_VERSION}"
$KUBECTL apply -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml"
$KUBECTL -n kube-system rollout status ds/calico-node --timeout=300s
$KUBECTL wait --for=condition=Ready nodes --all --timeout=300s
$KUBECTL -n kube-system rollout status deploy/coredns --timeout=180s

# The operator writes Kyverno Policy and ArgoCD Application objects. Only their
# CRDs are installed: this test is about the egress policy, not about either
# controller.
log "installing the CRDs the operator writes to"
$KUBECTL apply -f "$REPO_ROOT/test/crds/argoproj.io_applications.yaml"
$KUBECTL apply -f "$REPO_ROOT/test/crds/kyverno.io_policies.yaml"
$KUBECTL create namespace argocd

log "building and loading the operator image"
docker build -t idp-operator:e2e "$REPO_ROOT"
kind load docker-image idp-operator:e2e --name "$CLUSTER"

log "installing the operator with -enforce-egress"
$KUBECTL apply -f "$REPO_ROOT/config/crd/"
$KUBECTL apply -f "$REPO_ROOT/config/manager/manager.yaml"
$KUBECTL apply -f "$REPO_ROOT/config/rbac/"
$KUBECTL -n idp-system set image deploy/idp-operator manager=idp-operator:e2e
# `default` is allowed to reach every service, only so the control probe in
# step 3 can get past the destination's ingress rules.
$KUBECTL -n idp-system patch deploy/idp-operator --type=json -p '[
  {"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"},
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"-enforce-egress"},
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"-allow-ingress-from-namespaces=client,default"}
]'
$KUBECTL -n idp-system rollout status deploy/idp-operator --timeout=180s

log "creating three services: client declares server, nobody declares other"
for svc in client server other; do
  extra=""
  if [ "$svc" = "client" ]; then
    extra="  network:
    allowEgressTo:
      - namespace: server"
  fi
  $KUBECTL apply -f - <<EOF
apiVersion: idp.jiung9.github.io/v1alpha1
kind: ServiceDeployment
metadata:
  name: ${svc}
  namespace: idp-system
spec:
  team: checkout
  gitops:
    repoURL: https://github.com/acme-corp/${svc}-deploy
    path: overlays/prod
${extra}
EOF
done
for svc in client server other; do
  $KUBECTL wait --for=jsonpath='{.status.phase}'=Provisioned \
    "servicedeployment/${svc}" -n idp-system --timeout=120s
  $KUBECTL get networkpolicy idp-egress -n "$svc" >/dev/null \
    || fail "idp-egress was not created in ${svc}"
done
ok "services provisioned, idp-egress present in each namespace"

log "starting the probe pods"
run_pod() { # <namespace> <name> <args...>
  local ns=$1 name=$2; shift 2
  $KUBECTL run "$name" -n "$ns" --image="$AGNHOST_IMAGE" \
    --restart=Never -- "$@"
  $KUBECTL wait --for=condition=Ready "pod/${name}" -n "$ns" --timeout=120s
}
run_pod server server-pod netexec --http-port=8080
run_pod other  other-pod  netexec --http-port=8080
run_pod client client-pod pause
run_pod default control-pod pause
SERVER_IP=$($KUBECTL get pod server-pod -n server -o jsonpath='{.status.podIP}')
OTHER_IP=$($KUBECTL get pod other-pod -n other -o jsonpath='{.status.podIP}')

# can_connect <namespace> <pod> <ip> — one TCP connect with a short timeout.
can_connect() {
  $KUBECTL exec -n "$1" "$2" -- /agnhost connect "$3:8080" --timeout=3s >/dev/null 2>&1
}

# A new NetworkPolicy takes a moment to reach the CNI, so the allowed case is
# retried. The blocked case is asserted only after the allowed one has passed,
# by which time the policy is in place.
log "1. client reaches the namespace it declared"
for _ in $(seq 1 30); do
  can_connect client client-pod "$SERVER_IP" && break
  sleep 2
done
can_connect client client-pod "$SERVER_IP" || fail "client could not reach server, which it declared"
ok "client -> server allowed"

log "2. client cannot reach the namespace it did not declare"
if can_connect client client-pod "$OTHER_IP"; then
  fail "client reached other without declaring it; -enforce-egress is not dropping traffic"
fi
ok "client -> other blocked"

log "3. control: a pod outside any egress policy reaches other"
can_connect default control-pod "$OTHER_IP" \
  || fail "control pod could not reach other; the block in step 2 may come from ingress, not egress"
ok "default -> other allowed, so the block in step 2 is the egress policy"

log "all egress checks passed"
