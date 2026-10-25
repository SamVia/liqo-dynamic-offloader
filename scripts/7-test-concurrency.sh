#!/bin/bash
set -euo pipefail

echo -e "\033[1;36m=== Testing Multi-Cluster Concurrency (Robust Local) ===\033[0m"

# Auto-setup: request two remote clusters; reuse them when they already exist.
bash ./scripts/1-setup.sh 2

BIN_PATH="/tmp/liqo-manager-test"
kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo -e "\n1. Building and Starting the Operator locally..."
go build -o "${BIN_PATH}" ./main.go

# Disable the Helm operator for safety and remove any local binaries.
pkill -9 -f "liqo-manager-test" || true
kubectl scale deployment liqo-dynamic-offloader -n default --replicas=0 --ignore-not-found=true >/dev/null 2>&1 || true

${BIN_PATH} \
  --leader-election-namespace="default" \
  --target-cluster-ids="cluster-remote,cluster-alpha" \
  --trap-backoff="5s" \
  --cleanup-delay="15s" > /tmp/liqo-concurrency.log 2>&1 &
OPERATOR_PID=$!

cleanup() {
  echo -e "\nCleaning up..."
  kill $OPERATOR_PID 2>/dev/null || true
  pkill -9 -f "liqo-manager-test" 2>/dev/null || true
  rm -f "${BIN_PATH}"
  kubectl delete namespace test-cluster-remote test-cluster-alpha --ignore-not-found=true >/dev/null 2>&1
}
trap cleanup EXIT

echo "Waiting for operator to start and acquire lease (10s)..."
sleep 10

echo -e "\n2. Setting up Test Environments..."
for ns in test-cluster-remote test-cluster-alpha; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl delete deployment trap-deployment -n "$ns" --ignore-not-found=true >/dev/null 2>&1
  kubectl delete namespaceoffloading offloading -n "$ns" --ignore-not-found=true >/dev/null 2>&1
done

echo -e "\n3. Deploying Pods targeting DIFFERENT remote clusters concurrently..."
# The two targets are now cluster-remote and cluster-alpha.
for cluster in cluster-remote cluster-alpha; do
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: trap-deployment
  namespace: test-$cluster
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
        kubernetes.io/hostname: $cluster
      tolerations:
      - key: "virtual-node.liqo.io/not-allowed"
        operator: "Exists"
EOF
done

echo -e "\nWaiting up to 120 seconds for pods to recover from OffloadingBackOff and become Running..."
for i in {1..60}; do
  REMOTE_READY=$(kubectl get pods -n test-cluster-remote -l app=trap -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || echo "false")
  ALPHA_READY=$(kubectl get pods -n test-cluster-alpha -l app=trap -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || echo "false")

  if [[ "$REMOTE_READY" == "true" && "$ALPHA_READY" == "true" ]]; then
    echo -e "\n\033[1;32mBoth pods are successfully offloaded and Running!\033[0m"
    break
  fi
  sleep 2
done

echo -e "\n\033[1;36m=== Evaluation Results & Final State ===\033[0m"

for cluster in cluster-remote cluster-alpha; do
  ns="test-$cluster"
  echo -e "\n------------------------------------------------------------"
  echo -e "Status for Namespace: \033[1;33m$ns\033[0m (Targeting: $cluster)"
  echo -e "------------------------------------------------------------"
  
  echo "Pods:"
  kubectl get pods -n "$ns" -o wide
  
  echo -e "\nNamespaceOffloading Policy:"
  kubectl get namespaceoffloading offloading -n "$ns" -o yaml | grep -A 8 "spec:" || echo "No policy found."
  
  echo -e "\nResult:"
  POD_PHASE=$(kubectl get pods -n "$ns" -l app=trap -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "Unknown")
  if kubectl get namespaceoffloading offloading -n "$ns" >/dev/null 2>&1; then
    if [[ "$POD_PHASE" == "Running" ]]; then
      echo -e "[\033[1;32mSUCCESS\033[0m] Policy created and pod is Running on $cluster."
    else
      echo -e "[\033[1;31mPARTIAL\033[0m] Policy created, but pod is still $POD_PHASE."
    fi
  else
    echo -e "[\033[1;31mFAIL\033[0m] No Policy found for $cluster."
  fi
done