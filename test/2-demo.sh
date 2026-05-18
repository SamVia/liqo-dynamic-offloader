#!/bin/bash

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "Step 1: Generating the Deep Trap Deployment YAML..."
cat <<EOF > /tmp/trap-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: liqo-trap-deployment
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: liqo-trap
  template:
    metadata:
      labels:
        app: liqo-trap
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
      nodeSelector:
        kubernetes.io/hostname: cluster-remote
      tolerations:
      - key: "virtual-node.liqo.io/not-allowed"
        operator: "Exists"
EOF

echo "Cleaning up any previous runs..."
kubectl delete deployment liqo-trap-deployment -n default --ignore-not-found=true >/dev/null 2>&1
kubectl delete namespaceoffloading offloading -n default --ignore-not-found=true >/dev/null 2>&1

sleep 2

echo "Step 2: Starting Telemetry Monitors in the background..."
echo "---------------------------------------------------------------------"

trap 'echo -e "\n Demo terminated. Cleaning up background processes..."; kill $(jobs -p) 2>/dev/null; exit' SIGINT SIGTERM

# Watch the pods normally so the audience clearly sees the 'OffloadingBackOff' status!
kubectl get pods -w | grep --line-buffered "liqo-trap" &

kubectl get events --field-selector involvedObject.kind=Pod --watch-only \
  -o custom-columns="REASON:.reason,MESSAGE:.message" | \
  awk '
    /ReflectionDisabled/ {
      print "\n\033[1;31m VIRTUAL KUBELET REJECTION DETECTED \033[0m"
      print "\033[1;31mReason:\033[0m  ReflectionDisabled"
      print "\033[1;31mMessage:\033[0m " substr($0, index($0, $2)) "\n"
    }
  ' &

sleep 2

echo -e "\n Step 3: Springing the trap (Deploying pod to non-offloaded namespace)..."
kubectl apply -f /tmp/trap-deployment.yaml

echo -e "\n Waiting for the Virtual Kubelet to react (Press CTRL+C to exit demo)...\n"

wait    