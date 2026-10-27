#!/usr/bin/env bash
# Install the working tree's build twice on one cluster as README says,
# without d2k-tls Secrets (d2k creates its certificates), then uninstall one
# install: the other keeps working and keeps the shared admission policy.
#
#   test/e2e/install-lifecycle.sh
#
# CLUSTER picks another kind cluster.
set -uo pipefail
. "$(dirname "$0")/upgrade-lib.sh"

echo "== build the working tree"
image working-tree lifecycle || { echo "build failed"; exit 1; }

echo "== install e2e-a and e2e-b"
k apply -f "$ROOT/deploy/admission-policy.yaml" >/dev/null
for ns in e2e-a e2e-b; do
	render "$ROOT/deploy/kubernetes.yaml" "$ns" lifecycle false >"$WORKDIR/$ns.yaml"
	install "$ROOT/deploy/kubernetes.yaml" "$ns" lifecycle false || { echo "install $ns failed"; exit 1; }
done
check "e2e-a: d2k created its certificates" k -n e2e-a get secret d2k-tls d2k-client
check "e2e-a: README's client files connect" connect e2e-a 2376 26780 "$(client_cert e2e-a)"
check "e2e-a: docker run" bash -c '[ "$(docker run --rm busybox:1.36 echo hi)" = hi ]'
check "e2e-b: its own certificates connect" connect e2e-b 2376 26781 "$(client_cert e2e-b)"
check "e2e-b refuses e2e-a's client certificate" bash -c "! DOCKER_CERT_PATH=$WORKDIR/client-e2e-a docker version"
check "e2e-b: network create (patches its namespace)" docker network create netb

echo "== uninstall e2e-a"
check "kubectl delete -f e2e-a's manifest" k delete -f "$WORKDIR/e2e-a.yaml" --wait=true --timeout=180s
check "admission policy still installed" k get validatingadmissionpolicy,validatingadmissionpolicybinding d2k-namespace-network-annotations
check "e2e-b still serves" connect e2e-b 2376 26782 "$WORKDIR/client-e2e-b"
check "e2e-b: network create after the uninstall" docker network create netb2
check "e2e-b: networks kept" bash -c "docker network ls --format '{{.Name}}' | grep -q netb2"
check "e2e-b's ServiceAccount still can't change its namespace's labels" bash -c "! kubectl --context $CONTEXT label ns e2e-b test=x --as system:serviceaccount:e2e-b:d2k"
result
