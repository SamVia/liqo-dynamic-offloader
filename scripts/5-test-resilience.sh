#!/bin/bash
set -euo pipefail

echo -e "\033[1;36m=== Testing Controller Resilience (Robust Local Restart) ===\033[0m"
DEMO_REMOTE_CLUSTER="${DEMO_REMOTE_CLUSTER:-cluster-remote}"
BIN_PATH="/tmp/liqo-manager-test"
OPERATOR_PID=""
ORIGINAL_OPERATOR_REPLICAS=""
ORIGINAL_LEGACY_OPERATOR_REPLICAS=""
OPERATOR_STATE_FILE="/tmp/liqo-resilience-deployments.$$"

kubectl config use-context kind-cluster-local >/dev/null 2>&1

wait_for_operator_pods_to_stop() {
  for i in {1..30}; do
    operator_pods="$(kubectl get pods -A \
      -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.containers[*].image' \
      --no-headers 2>/dev/null |
      grep -Ei 'liqo-dynamic-offloader|liqo-auto-healing-operator' || true)"
    if [ -z "${operator_pods}" ]; then
      return 0
    fi
    sleep 2
  done
  echo -e "[\033[1;31mFAIL\033[0m] Existing operator pods did not stop."
  return 1
}

crash_local_operator() {
  local pid args

  ps -eo pid=,args= 2>/dev/null |
    while read -r pid args; do
      [ -n "${pid}" ] || continue
      [ "${pid}" = "$$" ] && continue
      case "${args}" in
        *--leader-election-namespace=default*|*"go run"*--leader-election-namespace=default*)
          kill -9 "${pid}" 2>/dev/null || true
          ;;
      esac
    done || true
}

crash_cluster_operator() {
  # Stop local development instances as well as in-cluster deployments.
  crash_local_operator

  # Stop every matching deployment, including releases installed in another
  # namespace or with a custom Helm release name.
  : > "${OPERATOR_STATE_FILE}"
  kubectl get deployments -A \
    -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.template.spec.containers[*].image,REPLICAS:.spec.replicas' \
    --no-headers 2>/dev/null |
    grep -Ei 'liqo-dynamic-offloader|liqo-auto-healing-operator' |
    awk '{print $1, $2, $4}' |
    while read -r namespace deployment replicas; do
      [ -n "${namespace}" ] && [ -n "${deployment}" ] || continue
      printf '%s %s %s\n' "${namespace}" "${deployment}" "${replicas}" >> "${OPERATOR_STATE_FILE}"
      kubectl scale deployment "${deployment}" -n "${namespace}" --replicas=0 \
        --ignore-not-found=true >/dev/null 2>&1 || true
    done || true

  # Simulate an abrupt process/node failure rather than a graceful shutdown.
  kubectl get pods -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.containers[*].image' \
    --no-headers 2>/dev/null |
    grep -Ei 'liqo-dynamic-offloader|liqo-auto-healing-operator' |
    awk '{print $1, $2}' |
    while read -r namespace pod; do
      [ -n "${namespace}" ] && [ -n "${pod}" ] || continue
      kubectl delete pod "${pod}" -n "${namespace}" \
        --grace-period=0 \
        --force \
        --ignore-not-found=true >/dev/null 2>&1 || true
    done || true

  wait_for_operator_pods_to_stop
}

cleanup() {
  echo -e "\nCleaning up..."
  if [ -n "${OPERATOR_PID}" ]; then
    kill "${OPERATOR_PID}" 2>/dev/null || true
  fi
  rm -f "${BIN_PATH}"
  if [ -f "${OPERATOR_STATE_FILE}" ]; then
    while read -r namespace deployment replicas; do
      [ -n "${namespace}" ] && [ -n "${deployment}" ] || continue
      kubectl scale deployment "${deployment}" -n "${namespace}" --replicas="${replicas}" \
        --ignore-not-found=true >/dev/null 2>&1 || true
    done < "${OPERATOR_STATE_FILE}"
    rm -f "${OPERATOR_STATE_FILE}"
  fi
  if [ -n "${ORIGINAL_OPERATOR_REPLICAS}" ]; then
    kubectl scale deployment liqo-dynamic-offloader \
      -n default \
      --replicas="${ORIGINAL_OPERATOR_REPLICAS}" \
      --ignore-not-found=true >/dev/null 2>&1 || true
  fi
  if [ -n "${ORIGINAL_LEGACY_OPERATOR_REPLICAS}" ]; then
    kubectl scale deployment liqo-auto-healing-operator \
      -n default \
      --replicas="${ORIGINAL_LEGACY_OPERATOR_REPLICAS}" \
      --ignore-not-found=true >/dev/null 2>&1 || true
  fi
  kubectl delete namespace test-resilience --ignore-not-found=true >/dev/null 2>&1 || true
}
trap cleanup EXIT

bash ./scripts/1-setup.sh 1

echo -e "\n1. Building the Operator binary..."
# Compile explicitly to retain full control over the process.
go build -o "${BIN_PATH}" ./main.go

echo -e "\n2. Setting up Test Environment..."
# Additional safety: terminate any lingering local binaries.
crash_local_operator
# Additional in-cluster cleanup if the Helm operator is installed.
ORIGINAL_OPERATOR_REPLICAS="$(kubectl get deployment liqo-dynamic-offloader -n default \
  -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
ORIGINAL_LEGACY_OPERATOR_REPLICAS="$(kubectl get deployment liqo-auto-healing-operator -n default \
  -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
crash_cluster_operator
kubectl delete lease liqo-auto-healing-lock -n default \
  --ignore-not-found=true >/dev/null 2>&1 || true
kubectl create namespace test-resilience --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl delete deployment trap-deployment -n test-resilience --ignore-not-found=true >/dev/null 2>&1
kubectl delete namespaceoffloading offloading -n test-resilience --ignore-not-found=true >/dev/null 2>&1
for i in {1..30}; do
  if ! kubectl get namespaceoffloading offloading -n test-resilience >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
if kubectl get namespaceoffloading offloading -n test-resilience >/dev/null 2>&1; then
  echo -e "[\033[1;31mFAIL\033[0m] Could not remove stale NamespaceOffloading policy."
  exit 1
fi

echo -e "\n3. Springing a trap while Operator is OFFLINE..."
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: trap-deployment
  namespace: test-resilience
spec:
  replicas: 1
  selector:
    matchLabels:
      app: trap
  template:
    metadata:
      labels:
        app: trap
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
      nodeSelector:
        kubernetes.io/hostname: ${DEMO_REMOTE_CLUSTER}
      tolerations:
      - key: "virtual-node.liqo.io/not-allowed"
        operator: "Exists"
EOF

echo "Waiting 10 seconds. The pod should remain stuck with NO policy created..."
sleep 10
if kubectl get namespaceoffloading offloading -n test-resilience >/dev/null 2>&1; then
  echo -e "[\033[1;31mFAIL\033[0m] Policy created while operator was dead? Something is wrong."
  kubectl get namespaceoffloading offloading -n test-resilience -o yaml || true
  echo "Potential remaining controllers:"
  kubectl get pods -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.containers[*].image' \
    --no-headers 2>/dev/null |
    grep -Ei 'liqo-dynamic-offloader|liqo-auto-healing-operator' || true
  exit 1
else
  echo -e "[\033[1;32mSUCCESS\033[0m] No policy exists, as expected."
fi

echo -e "\n4. Starting the Operator locally..."
# Start the compiled binary directly.
${BIN_PATH} \
  --leader-election-namespace="default" \
  --target-cluster-ids="${DEMO_REMOTE_CLUSTER}" \
  --trap-backoff="5s" \
  --cleanup-delay="15s" > /tmp/liqo-resilience.log 2>&1 &
OPERATOR_PID=$!

echo "Waiting for operator to start and acquire lease (up to 10s)..."
sleep 10
if ! kill -0 "${OPERATOR_PID}" 2>/dev/null; then
  echo -e "[\033[1;31mFAIL\033[0m] Operator exited during startup. See /tmp/liqo-resilience.log."
  exit 1
fi

echo -e "\n5. Waiting for the Operator to reconcile the existing stuck pod on boot..."
for i in {1..15}; do
  if kubectl get namespaceoffloading offloading -n test-resilience >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

if kubectl get namespaceoffloading offloading -n test-resilience >/dev/null 2>&1; then
  echo -e "[\033[1;32mSUCCESS\033[0m] Operator successfully processed the backlog and created the policy!"
else
  echo -e "[\033[1;31mFAIL\033[0m] Operator did not create policy upon restart."
  exit 1
fi