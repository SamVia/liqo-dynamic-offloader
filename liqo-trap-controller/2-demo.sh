#!/bin/bash

kubectl config use-context kind-cluster-local >/dev/null 2>&1

echo "Step 1: Generating the Deep Trap YAML..."
cat <<EOF > /tmp/trap-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: liqo-trap-pod
  namespace: default
spec:
  containers:
  - name: nginx
    image: nginx:alpine
  nodeSelector:
    liqo.io/type: virtual-node
  # THE VIP PASS: We tolerate the taint to bypass Kube-scheduler 
  # and force the pod directly into the Virtual Kubelet's domain.
  tolerations:
  - key: "virtual-node.liqo.io/not-allowed"
    operator: "Exists"
EOF

echo "Cleaning up any previous runs..."
kubectl delete pod liqo-trap-pod --ignore-not-found=true --wait=true >/dev/null 2>&1

echo "Step 2: Starting Telemetry Monitors in the background..."
echo "---------------------------------------------------------------------"

trap 'echo -e "\nDemo terminated. Cleaning up background processes..."; kill $(jobs -p) 2>/dev/null; exit' SIGINT SIGTERM

# Task A: Watch ALL pods to prevent "NotFound" crashes, filter for our trap
kubectl get pods -w \
  -o custom-columns="TIME:.metadata.creationTimestamp,NAME:.metadata.name,PHASE:.status.phase" | grep --line-buffered "liqo-trap-pod" &

# Task B: Watch events for the Virtual Kubelet's ReflectionDisabled event
kubectl get events --field-selector involvedObject.name=liqo-trap-pod --watch-only \
  -o custom-columns="REASON:.reason,MESSAGE:.message" | \
  awk '
    /ReflectionDisabled/ {
      print "\n\033[1;31m VIRTUAL KUBELET REJECTION DETECTED \033[0m"
      print "\033[1;31mReason:\033[0m  ReflectionDisabled"
      print "\033[1;31mMessage:\033[0m " substr($0, index($0, $2)) "\n"
    }
    /FailedScheduling/ {
      print "\n\033[1;33m  SCHEDULER WARNING \033[0m"
      print "\033[1;33mReason:\033[0m  FailedScheduling"
      print "\033[1;33mMessage:\033[0m " substr($0, index($0, $2)) "\n"
    }
    !/ReflectionDisabled/ && !/FailedScheduling/ {
      if (NR>1) print "  " $0
    }
  ' &

sleep 2

echo -e "\n Step 3: Springing the trap (Deploying pod to non-offloaded namespace)..."
kubectl apply -f /tmp/trap-pod.yaml

echo -e "\n Waiting for the Virtual Kubelet to react (Press CTRL+C to exit demo)...\n"
wait
