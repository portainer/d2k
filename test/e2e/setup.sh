#!/usr/bin/env bash
# Builds d2k from the working tree, loads it into a kind cluster and deploys
# two instances from deploy/kubernetes.yaml:
#   standalone  namespace d2k        NodePort 31112
#   swarm       namespace d2k-swarm  NodePort 31111
# The kind cluster must map both ports to the host (see test/e2e/README.md).
# Both instances serve mutual TLS with certificates from `d2k pki init`, kept
# in test/e2e/.pki; run.sh uses the client certificate from there.
set -euo pipefail

CLUSTER=${CLUSTER:-d2k}
CONTEXT=kind-$CLUSTER
IMAGE=${IMAGE:-d2k:dev}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
PKI=$ROOT/test/e2e/.pki

[ -d "$PKI" ] || (cd "$ROOT" && go run ./cmd pki init --san localhost,127.0.0.1 --out "$PKI" >/dev/null)

docker build -t "$IMAGE" --build-arg VERSION=dev "$ROOT"
kind load docker-image "$IMAGE" --name "$CLUSTER"

# render <namespace> <swarm mode> <node port>
render() {
  sed -e "s/^\(  name: \)d2k\(   # change to your target namespace\)/\1$1\2/" \
      -e "s/namespace: d2k   #/namespace: $1   #/" \
      -e "s/d2k-node-reader/d2k-node-reader-$1/" \
      -e "s#image: portainer/d2k:latest#image: $IMAGE#" \
      -e "s/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/" \
      -e "s/value: \"true\"   # set to \"false\"/value: \"$2\"   # set to \"false\"/" \
      -e "s/^      targetPort: 2376$/      targetPort: 2376\n      nodePort: $3/" \
      -e "s/^  type: LoadBalancer$/  type: NodePort/" \
      "$ROOT/deploy/kubernetes.yaml"
}

deploy() {
  kubectl --context "$CONTEXT" create namespace "$1" --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f -
  kubectl --context "$CONTEXT" -n "$1" create secret generic d2k-tls --from-file="$PKI/server" --dry-run=client -o yaml |
    kubectl --context "$CONTEXT" apply -f -
  render "$1" "$2" "$3" | kubectl --context "$CONTEXT" apply -f -
  # Pick up a rebuilt image under the same tag.
  kubectl --context "$CONTEXT" -n "$1" rollout restart deployment/d2k
  kubectl --context "$CONTEXT" -n "$1" rollout status deployment/d2k --timeout=120s
}

deploy d2k false 31112
deploy d2k-swarm true 31111
