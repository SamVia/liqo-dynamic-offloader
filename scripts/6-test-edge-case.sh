#!/bin/bash
set -euo pipefail

echo -e "\033[1;36m=== Testing Edge Case: Pod Dies without NamespaceOffloading (Local) ===\033[0m"
bash ./scripts/1-setup.sh 1
DEMO_REMOTE_CLUSTER="${DEMO_REMOTE_CLUSTER:-cluster-remote}"

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo -e "\n1. Starting the Operator locally (via go run)..."
pkill -f "go run ./main.go" || true
kubectl scale deployment liqo-dynamic-offloader -n default --replicas=0 --ignore-not-found=true >/dev/null 2>&1 || true

go run ./main.go \
  --leader-election-namespace="default" \
  --target-cluster-ids="${DEMO_REMOTE_CLUSTER}" \
  --trap-backoff="5s" \
  --cleanup-delay="15s" > /tmp/liqo-edge-case.log 2>&1 &
OPERATOR_PID=$!

cleanup() {
  echo -e "\nCleaning up..."
  kill $OPERATOR_PID 2>/dev/null || true
  pkill -f "exe/main" 2>/dev/null || true
  kubectl delete namespace test-edge-case --ignore-not-found=true >/dev/null 2>&1
}
trap cleanup EXIT

echo "Waiting for operator to start and acquire lease (10s)..."
sleep 10

echo -e "\n2. Setting up Test Environment..."
kubectl create namespace test-edge-case --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl delete deployment trap-deployment -n test-edge-case --ignore-not-found=true >/dev/null 2>&1
kubectl delete namespaceoffloading offloading -n test-edge-case --ignore-not-found=true >/dev/null 2>&1

echo -e "\n3. Springing the trap..."
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: trap-deployment
  namespace: test-edge-case
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

echo -e "\n4. Immediately deleting the pod to trigger the edge case..."
kubectl delete deployment trap-deployment -n test-edge-case >/dev/null 2>&1

echo "Waiting for 10 seconds to let the operator process the event..."
sleep 10

echo -e "\n5. Validating Operator Logs..."
if grep -qi "panic" /tmp/liqo-edge-case.log; then
    echo -e "[\033[1;31mFAIL\033[0m] Panic found in logs!"
    cat /tmp/liqo-edge-case.log
else
    echo -e "[\033[1;32mSUCCESS\033[0m] No panics detected. Edge Case handled gracefully!"
fi