#!/bin/bash
# Quick test: can d2k deploy GPU workloads?
# Needs: minikube + AMD GPU device plugin + d2k port-forwarded to :2375

D2K="docker -H tcp://localhost:2375"
NS="d2k"

echo "--- checking d2k is up ---"
$D2K info > /dev/null 2>&1 || { echo "d2k not reachable"; exit 1; }
echo "ok"

echo ""
echo "--- checking GPUs on node ---"
kubectl get nodes -o json | jq '.items[0].status.allocatable["amd.com/gpu"]'

# test 1: deploy a container through d2k, check for GPU devices
echo ""
echo "--- deploy via d2k (no gpu flags) ---"
$D2K run -d --name gpu-test alpine:latest sleep 120
sleep 8

POD=$(kubectl get pods -n $NS -l app=gpu-test -o jsonpath='{.items[0].metadata.name}')
echo "pod: $POD"
echo ""
echo "resources d2k set:"
kubectl get deploy gpu-test -n $NS -o jsonpath='{.spec.template.spec.containers[0].resources}'
echo ""
echo ""
echo "looking for /dev/kfd:"
kubectl exec -n $NS $POD -- ls -la /dev/kfd 2>&1
echo ""
echo "looking for /dev/dri:"
kubectl exec -n $NS $POD -- ls -la /dev/dri/ 2>&1

$D2K rm -f gpu-test
sleep 3

# test 2: deploy the same thing directly via kubectl WITH gpu resource
echo ""
echo "--- deploy via kubectl WITH amd.com/gpu: 1 (control) ---"
cat <<EOF | kubectl apply -n $NS -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-direct
spec:
  replicas: 1
  selector:
    matchLabels:
      app: gpu-direct
  template:
    metadata:
      labels:
        app: gpu-direct
    spec:
      containers:
      - name: alpine
        image: alpine:latest
        command: ["sleep", "120"]
        resources:
          limits:
            amd.com/gpu: 1
EOF

kubectl wait --for=condition=ready pod -l app=gpu-direct -n $NS --timeout=60s
DIRECT_POD=$(kubectl get pods -n $NS -l app=gpu-direct -o jsonpath='{.items[0].metadata.name}')
echo "pod: $DIRECT_POD"
echo ""
echo "resources set:"
kubectl get deploy gpu-direct -n $NS -o jsonpath='{.spec.template.spec.containers[0].resources}'
echo ""
echo ""
echo "looking for /dev/kfd:"
kubectl exec -n $NS $DIRECT_POD -- ls -la /dev/kfd 2>&1
echo ""
echo "looking for /dev/dri:"
kubectl exec -n $NS $DIRECT_POD -- ls -la /dev/dri/ 2>&1

# cleanup
echo ""
echo "--- cleanup ---"
kubectl delete deploy gpu-direct -n $NS 2>/dev/null
echo "done"
