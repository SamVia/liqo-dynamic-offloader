#!/bin/bash
set -e

echo -e "\033[1;36m=== Local Operator Demo (Dynamic Polling) ===\033[0m"
bash ./scripts/1-setup.sh 1
DRY_RUN="false"
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN="true"
  echo -e "\033[1;33m>>> DRY-RUN MODE ENABLED <<<\033[0m"
fi

TARGET_CLUSTERS="${TARGET_CLUSTER_ID:-cluster-remote}"
DEMO_REMOTE_CLUSTER="${DEMO_REMOTE_CLUSTER:-cluster-remote}"

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo -e "\n1. Preparing Test Namespaces..."
kubectl create namespace demo-allowed --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create namespace demo-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Namespace
metadata:
  name: demo-labeled
  labels:
    dynamic-offloader.liqo.io/ignore-trap: "true"
EOF

# Perform preliminary cleanup.
for ns in demo-allowed demo-system demo-labeled; do
  kubectl delete deployment trap-deployment -n $ns --ignore-not-found=true >/dev/null 2>&1
  kubectl delete namespaceoffloading offloading -n $ns --ignore-not-found=true >/dev/null 2>&1
done

echo -e "\n2. Starting the Operator locally (via go run)..."
# Pass the new --leader-election-namespace flag.
go run ./main.go \
  --leader-election-namespace="default" \
  --target-cluster-ids="${TARGET_CLUSTERS}" \
  --excluded-namespaces="*-system,local-path-storage" \
  --trap-blacklist-labels="dynamic-offloader.liqo.io/ignore-trap=true" \
  --trap-backoff="5s" \
  --cleanup-delay="15s" \
  --dry-run="${DRY_RUN}" > /tmp/liqo-operator-local.log 2>&1 &

OPERATOR_PID=$!
trap 'echo -e "\nDemo terminated. Killing operator and cleaning up..."; kill $OPERATOR_PID 2>/dev/null; kubectl delete namespace demo-allowed demo-system demo-labeled --ignore-not-found=true >/dev/null 2>&1; exit' SIGINT SIGTERM EXIT

echo "Waiting for the operator to compile and acquire leader lease..."
timeout=60
while ! grep -q "successfully acquired lease\|Starting workers" /tmp/liqo-operator-local.log; do
  sleep 1
  timeout=$((timeout-1))
  if [ $timeout -le 0 ]; then
    echo -e "\033[1;31mTimeout waiting for operator to start. Check logs:\033[0m"
    cat /tmp/liqo-operator-local.log
    exit 1
  fi
done
echo -e "\033[1;32mOperator is ready!\033[0m"

echo -e "\n\033[1;33m>>> Starting Pod Watcher in 'demo-allowed' <<<\033[0m"
kubectl get pods -n demo-allowed -w &
WATCH_PID=$!

echo -e "\n3. PHASE 1: Springing the traps across all namespaces..."
for ns in demo-allowed demo-system demo-labeled; do
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: trap-deployment
  namespace: $ns
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
done

echo -e "\nWaiting for Liqo to reject the pod and the Operator to react (up to 30s)..."
if [[ "$DRY_RUN" == "false" ]]; then
  for i in {1..15}; do
    if kubectl get namespaceoffloading offloading -n demo-allowed >/dev/null 2>&1; then
      break
    fi
    sleep 2
  done
else
  sleep 15
fi

echo -e "\n\033[1;36m=== Phase 1 Results (Trap Creation) ===\033[0m"
for ns in demo-allowed demo-system demo-labeled; do
  if kubectl get namespaceoffloading offloading -n "$ns" >/dev/null 2>&1; then
    echo -e "[\033[1;32mYES\033[0m] $ns -> Offloading Policy Created"
  else
    echo -e "[\033[1;31mNO\033[0m]  $ns -> No Policy (Filtered or Dry-Run)"
  fi
done

if curl -s --max-time 1 http://localhost:8080/metrics >/dev/null 2>&1; then
  echo -e "\n\033[1;33m>>> Prometheus Metrics after Phase 1 <<<\033[0m"
  curl -s http://localhost:8080/metrics | grep -E '^liqo_' || true
fi

echo -e "\n4. PHASE 2: Testing Cleanup Deletion..."
if [[ "$DRY_RUN" == "false" ]]; then
  echo "Deleting the stuck pod from 'demo-allowed' to trigger cleanup countdown..."
  kubectl delete deployment trap-deployment -n demo-allowed >/dev/null 2>&1
  
  # The countdown starts only once the remote pod has fully terminated, and
  # Liqo's finalizer keeps the policy briefly after deletion.
  echo "Waiting for the pod to terminate, the 15s timer to expire and the policy to be deleted (up to 60s)..."
  for i in {1..30}; do
    if ! kubectl get namespaceoffloading offloading -n demo-allowed >/dev/null 2>&1; then
      break
    fi
    sleep 2
  done
  
  echo -e "\n\033[1;36m=== Phase 2 Results (Cleanup Deletion) ===\033[0m"
  if kubectl get namespaceoffloading offloading -n demo-allowed >/dev/null 2>&1; then
    echo -e "[\033[1;31mFAIL\033[0m] demo-allowed -> Policy is STILL THERE (Cleanup failed!)"
  else
    echo -e "[\033[1;32mSUCCESS\033[0m] demo-allowed -> Policy was successfully DELETED."
  fi
else
  echo "Skipping Phase 2 because Dry-Run is enabled (no policy was actually created)."
fi

# Stop watching pods
kill "${WATCH_PID:-}" 2>/dev/null || true

echo -e "\n5. PHASE 3: Prometheus Metrics Final Snapshot..."
if curl -s --max-time 1 http://localhost:8080/metrics >/dev/null 2>&1; then
  echo -e "\033[1;32m>>> Prometheus Metrics (:8080/metrics) <<<\033[0m"
  curl -s http://localhost:8080/metrics | grep -E '^liqo_' || true
fi

echo -e "\n---------------------------------------------------------------------"
echo "Streaming Operator Logs (Press CTRL+C to exit and cleanup)..."
echo "---------------------------------------------------------------------"
cat /tmp/liqo-operator-local.log