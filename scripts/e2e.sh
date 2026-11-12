#!/bin/bash
# End-to-end check against a real API server: creates a kind cluster, installs
# the Gateway API and agentgateway CRDs (no controller), applies
# examples/demo.yaml, runs agwmap against it and checks what comes out.
#
#   scripts/e2e.sh
#
# Needs kind, kubectl, helm, jq and go. The cluster is deleted at the end; set
# KEEP_CLUSTER=1 to keep it.
set -euo pipefail

GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.6.3}"
AGENTGATEWAY_VERSION="${AGENTGATEWAY_VERSION:-v1.7.0}"
CLUSTER=agwmap-e2e
CONTEXT="kind-${CLUSTER}"

cd "$(dirname "$0")/.."
work="$(mktemp -d)"
cleanup() {
  rm -rf "$work"
  if [ -z "${KEEP_CLUSTER:-}" ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
# check <description> <jq filter that must print true> <file>
check() {
  [ "$(jq -r "$2" "$3")" = true ] || fail "$1 ($2)"
  echo "ok: $1"
}

go build -o "$work/kubectl-agwmap" .
# A kubeconfig of its own: kind would otherwise switch, and on deletion unset,
# the current context of the one in use.
export KUBECONFIG="$work/kubeconfig"
agwmap() { "$work/kubectl-agwmap" --context "$CONTEXT" "$@"; }

kind create cluster --name "$CLUSTER" --wait 120s
kubectl --context "$CONTEXT" apply --server-side -f \
  "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null
helm upgrade -i agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
  --kube-context "$CONTEXT" --create-namespace --namespace agentgateway-system \
  --version "$AGENTGATEWAY_VERSION" >/dev/null
kubectl --context "$CONTEXT" wait --for=condition=Established crd --all --timeout=60s >/dev/null
kubectl --context "$CONTEXT" apply -f examples/demo.yaml >/dev/null

# The real values: every object of the example, each one once.
agwmap -anonymize=false -o "$work/raw.json"
kinds='[.objects[] | select(.metadata.namespace // "" | startswith("agwmap-demo")) | .kind] | sort | join(",")'
want="AgentgatewayBackend,AgentgatewayPolicy,AgentgatewayPolicy,Gateway,HTTPRoute,HTTPRoute,ReferenceGrant"
check "captures every object of the example once" "$kinds == \"$want\"" "$work/raw.json"
check "names the kube context" ".meta.context == \"$CONTEXT\" and .meta.anonymized == false" "$work/raw.json"
check "has no warnings" '(.meta.warnings // []) | length == 0' "$work/raw.json"
check "drops server-side metadata" '[.objects[].metadata | keys[] | select(IN("name","namespace","labels","annotations") | not)] | length == 0' "$work/raw.json"
check "keeps the real hostnames" '[.objects[] | select(.kind == "Gateway") | .spec.listeners[].hostname] | index("*.demo-company.test") != null' "$work/raw.json"

# The default: anonymized.
agwmap -o "$work/anon.json"
check "is anonymized by default" '.meta.anonymized and .meta.context == "cluster"' "$work/anon.json"
if grep -q -e demo-company -e "$CONTEXT" "$work/anon.json"; then fail "the anonymized snapshot still holds a real value"; fi
echo "ok: the anonymized snapshot holds no hostname of the example"
check "keeps names and namespaces" '[.objects[] | select(.kind == "HTTPRoute" and .metadata.namespace == "agwmap-demo") | .metadata.name] | sort == ["chat","shop"]' "$work/anon.json"

# One namespace.
agwmap -n agwmap-demo-reviews -anonymize=false -o "$work/ns.json"
check "-n reads one namespace" '[.objects[] | select(.metadata.namespace) | .metadata.namespace] | unique == ["agwmap-demo-reviews"]' "$work/ns.json"

# The page, straight from the cluster and from a snapshot: same graph.
graph() { sed -n 's/.*<script id="data" type="application\/json">\(.*\)<\/script>.*/\1/p' "$1" | jq -S '{nodes: [.nodes[].id], edges: [.edges[] | del(.note)]}'; }
agwmap -o "$work/map.html"
agwmap -from "$work/anon.json" -o "$work/from.html"
graph "$work/map.html" > "$work/map.graph"
graph "$work/from.html" > "$work/from.graph"
[ -s "$work/map.graph" ] || fail "the page holds no graph"
cmp -s "$work/map.graph" "$work/from.graph" || fail "the page from a snapshot differs from the page from the cluster"
echo "ok: a snapshot renders to the same graph as the cluster"
check "the cross-namespace backend is found, with its grant" \
  '[.edges[] | select(.type == "backend" and .crossNs == true and (.grant // "" | contains("shop-to-reviews")))] | length == 1' \
  <(sed -n 's/.*<script id="data" type="application\/json">\(.*\)<\/script>.*/\1/p' "$work/map.html")

echo "PASS"
