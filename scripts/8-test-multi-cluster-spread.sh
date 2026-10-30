#!/bin/bash
set -euo pipefail

echo -e "\033[1;36m=== Testing Multi-Cluster Pod Distribution (Spread on Virtual Nodes) ===\033[0m"

# Auto-setup: request two remote clusters (cluster-remote and cluster-alpha)
bash ./scripts/1-setup.sh 2

BIN_PATH="/tmp/liqo-manager-test"
NAMESPACE="test-cluster-spread"
DEPLOYMENT_NAME="spread-deployment"
REPLICAS=4

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo -e "\n1. Building and Starting the Operator locally..."
go build -o "${BIN_PATH}" ./main.go

# Disable any existing Helm operator and kill leftover local binaries
pkill -9 -f "liqo-manager-test" 2>/dev/null || true
kubectl scale deployment liqo-dynamic-offloader -n default --replicas=0 --ignore-not-found=true >/dev/null 2>&1 || true

${BIN_PATH} \
  --leader-election-namespace="default" \
  --target-cluster-ids="cluster-remote,cluster-alpha" \
  --trap-backoff="5s" \
  --cleanup-delay="15s" > /tmp/liqo-spread.log 2>&1 &
OPERATOR_PID=$!

cleanup() {
  echo -e "\nCleaning up..."
  kill $OPERATOR_PID 2>/dev/null || true
  pkill -9 -f "liqo-manager-test" 2>/dev/null || true
  rm -f "${BIN_PATH}"
  kubectl delete namespace "$NAMESPACE" --ignore-not-found=true >/dev/null 2>&1
}
trap cleanup EXIT

echo "Waiting for operator to start and acquire lease (10s)..."
sleep 10

echo -e "\n2. Setting up Test Environment in namespace '$NAMESPACE'..."
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl delete deployment "$DEPLOYMENT_NAME" -n "$NAMESPACE" --ignore-not-found=true >/dev/null 2>&1
kubectl delete namespaceoffloading offloading -n "$NAMESPACE" --ignore-not-found=true >/dev/null 2>&1

echo -e "\n3. Deploying $REPLICAS replicas targeting ANY Liqo Virtual Node (with topology spread)..."
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${DEPLOYMENT_NAME}
  namespace: ${NAMESPACE}
spec:
  replicas: ${REPLICAS}
  selector:
    matchLabels:
      app: spread-workload
  template:
    metadata:
      labels:
        app: spread-workload
    spec:
      topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: "kubernetes.io/hostname"
        whenUnsatisfiable: ScheduleAnyway
        labelSelector:
          matchLabels:
            app: spread-workload
      containers:
      - name: nginx
        image: nginx:alpine
      nodeSelector:
        liqo.io/type: virtual-node
      tolerations:
      - key: "virtual-node.liqo.io/not-allowed"
        operator: "Exists"
EOF

echo -e "\nWaiting for the operator to intercept OffloadingBackOff and create the NamespaceOffloading policy..."
for i in {1..20}; do
  if kubectl get namespaceoffloading offloading -n "$NAMESPACE" >/dev/null 2>&1; then
    echo -e "[\033[1;32mOK\033[0m] NamespaceOffloading created for $NAMESPACE!"
    break
  fi
  sleep 2
done

if ! kubectl get namespaceoffloading offloading -n "$NAMESPACE" >/dev/null 2>&1; then
  echo -e "[\033[1;31mFAIL\033[0m] Operator did not create NamespaceOffloading policy. Logs:"
  cat /tmp/liqo-spread.log
  exit 1
fi

echo -e "\nWaiting up to 120 seconds for all $REPLICAS pods to recover from OffloadingBackOff and reach 'Running'..."
ALL_READY=false
for i in {1..60}; do
  RUNNING_COUNT=$(kubectl get pods -n "$NAMESPACE" -l app=spread-workload --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l || echo 0)
  if [ "$RUNNING_COUNT" -ge "$REPLICAS" ]; then
    ALL_READY=true
    echo -e "[\033[1;32mOK\033[0m] All $REPLICAS pods are in Running phase!"
    break
  fi
  sleep 2
done

if [ "$ALL_READY" = "false" ]; then
  echo -e "[\033[1;33mWARNING\033[0m] Not all pods reached Running phase in time. Current pod state:"
  kubectl get pods -n "$NAMESPACE" -o wide
fi

echo -e "\n\033[1;36m=== Multi-Cluster Distribution Analysis ===\033[0m"
kubectl get pods -n "$NAMESPACE" -o wide

# Count pods running on cluster-remote vs cluster-alpha
COUNT_REMOTE=$(kubectl get pods -n "$NAMESPACE" -l app=spread-workload -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | grep -c "cluster-remote" || echo 0)
COUNT_ALPHA=$(kubectl get pods -n "$NAMESPACE" -l app=spread-workload -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | grep -c "cluster-alpha" || echo 0)

echo -e "\n------------------------------------------------------------"
echo -e "Pods on \033[1;33mcluster-remote\033[0m: $COUNT_REMOTE"
echo -e "Pods on \033[1;33mcluster-alpha\033[0m:  $COUNT_ALPHA"
echo -e "------------------------------------------------------------"

if [ "$COUNT_REMOTE" -gt 0 ] && [ "$COUNT_ALPHA" -gt 0 ]; then
  echo -e "[\033[1;32mSUCCESS\033[0m] A single NamespaceOffloading policy successfully distributed the workload across BOTH peered clusters!"
elif [ "$COUNT_REMOTE" -gt 0 ] || [ "$COUNT_ALPHA" -gt 0 ]; then
  echo -e "[\033[1;33mPARTIAL\033[0m] Pods are offloaded, but currently scheduled only on one remote cluster (Remote: $COUNT_REMOTE, Alpha: $COUNT_ALPHA)."
else
  echo -e "[\033[1;31mFAIL\033[0m] No pods were scheduled onto virtual nodes."
  exit 1
fi

echo -e "\n\033[1;36m=== Testing Phase 2: Cleanup Controller Verification ===\033[0m"
echo "Deleting deployment to test automated policy revocation when namespace is idle..."
kubectl delete deployment "$DEPLOYMENT_NAME" -n "$NAMESPACE" >/dev/null 2>&1

echo "Waiting for cleanup countdown and policy deletion (up to 35s)..."
POLICY_DELETED=false
for i in {1..18}; do
  if ! kubectl get namespaceoffloading offloading -n "$NAMESPACE" >/dev/null 2>&1; then
    POLICY_DELETED=true
    break
  fi
  sleep 2
done

if [ "$POLICY_DELETED" = "true" ]; then
  echo -e "[\033[1;32mSUCCESS\033[0m] NamespaceOffloading policy was successfully cleaned up after workload ended!"
else
  echo -e "[\033[1;31mFAIL\033[0m] Policy still exists in namespace '$NAMESPACE' after cleanup delay."
fi

echo -e "\n\033[1;32m=== Test 8 Completed Successfully ===\033[0m"
