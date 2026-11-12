#!/bin/bash
# Notices when the CRDs that agwmap reads change upstream. graph.go has drawing
# rules per kind, so a new kind or a changed field may need a change in the code.
# crds.lock.json records, for the latest release of each source, a fingerprint of
# every CRD; a scheduled job runs "check" and opens an issue when one differs.
#
#   scripts/crds.sh check [--report <file>]   compare the latest releases with the lock
#   scripts/crds.sh update                    rewrite crds.lock.json to the latest releases
#   scripts/crds.sh diff [<crd-name>]         print what changed in one CRD, or in all that changed
#
# The fingerprint is the SHA-256 of the CRD's spec without its description texts,
# as printed by "jq -S -c": a docs-only change is not drift, any structural change
# is. The lock holds names, versions and fingerprints only, never schema content:
# the enterprise chart is not ours to republish. "diff" downloads the schemas and
# prints to stdout, for local use.
#
# check exits 0 with or without drift and appends drift=true|false to
# $GITHUB_OUTPUT when that is set. GATEWAY_API_VERSION and
# AGENTGATEWAY_ENTERPRISE_VERSION pin a version in place of the latest one.
# GH_TOKEN or GITHUB_TOKEN, when set, authenticates the GitHub API call.
# Needs curl, jq, yq (mikefarah, v4) and helm.
set -euo pipefail

SOURCES="gateway-api-standard gateway-api-experimental agentgateway-enterprise"
GATEWAY_API_REPO="kubernetes-sigs/gateway-api"
CHART_REPO="us-docker.pkg.dev/solo-public/enterprise-agentgateway/charts/enterprise-agentgateway-crds"
LOCK="$(cd "$(dirname "$0")/.." && pwd)/crds.lock.json"

usage() { sed -n '7,9s/^# *//p' "$0"; }
log() { echo "crds: $*" >&2; }
die() { echo "crds: error: $*" >&2; exit 1; }
sha() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }

# The highest release among tags: vX.Y.Z, compared number by number, where
# vX.Y.Z-patch.N is newer than vX.Y.Z. Any other suffix (rc, beta, alpha) is skipped.
NEWEST='map(select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+(-patch\\.[0-9]+)?$"))) | max_by([scan("[0-9]+") | tonumber])'

gateway_api_latest() {
  local url="https://api.github.com/repos/${GATEWAY_API_REPO}/releases?per_page=100" token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
  if [ -n "$token" ]; then
    curl -fsSL --retry 3 -H "Authorization: Bearer $token" -o "$work/releases.json" "$url"
  else
    curl -fsSL --retry 3 -o "$work/releases.json" "$url"
  fi || die "cannot list the releases of ${GATEWAY_API_REPO}"
  jq -er "[.[] | select(.draft or .prerelease | not) | .tag_name] | $NEWEST" "$work/releases.json" ||
    die "no release found for ${GATEWAY_API_REPO}"
}

# The registry answers anonymously with every tag at once. Should it ever
# paginate, fail instead of picking the newest tag of the first page.
enterprise_latest() {
  curl -fsSL --retry 3 -D "$work/tags.headers" -o "$work/tags.json" "https://${CHART_REPO%%/*}/v2/${CHART_REPO#*/}/tags/list" ||
    die "cannot list the tags of ${CHART_REPO}"
  if grep -qi '^link:' "$work/tags.headers"; then die "the tag list of ${CHART_REPO} is paginated"; fi
  jq -er ".tags | $NEWEST" "$work/tags.json" || die "no release tag found for ${CHART_REPO}"
}

# fetch <source> <version>: writes $work/<source>@<version>.jsonl, one line per
# CRD with its name, kind, served versions and spec without descriptions. Only a
# description with a string value goes: a property named "description" stays.
fetch() {
  local raw="$work/$1@$2.yaml" out="$work/$1@$2.jsonl"
  if [ -s "$out" ]; then return 0; fi
  log "fetching $1 $2"
  case "$1" in
    gateway-api-*)
      curl -fsSL --retry 3 -o "$raw" \
        "https://github.com/${GATEWAY_API_REPO}/releases/download/$2/${1#gateway-api-}-install.yaml" ||
        die "cannot download $1 $2" ;;
    agentgateway-enterprise)
      helm template --version "$2" enterprise-agentgateway-crds "oci://${CHART_REPO}" > "$raw" 2>"$work/helm.err" ||
        die "cannot render $1 $2: $(cat "$work/helm.err")" ;;
  esac
  yq -o=json -I=0 'select(.kind == "CustomResourceDefinition")' "$raw" | jq -S -c '{
      name: .metadata.name,
      kind: .spec.names.kind,
      versions: [.spec.versions[] | select(.served) | .name] | sort,
      spec: .spec | walk(if type == "object" then with_entries(select(.key != "description" or (.value | type) != "string")) else . end)
    }' > "$out" || die "cannot parse $1 $2"
  [ -s "$out" ] || die "$1 $2 holds no CRD"
}

# lock_source <source> <version>: prints the lock entry of one source.
lock_source() {
  local meta spec
  fetch "$1" "$2"
  jq -S -c 'del(.spec), .spec' "$work/$1@$2.jsonl" | while IFS= read -r meta && IFS= read -r spec; do
    printf '%s\t%s\n' "$meta" "$(printf '%s\n' "$spec" | sha)"
  done | jq -R -n --arg version "$2" '{
      version: $version,
      crds: [inputs | split("\t") | (.[0] | fromjson) + {sha256: .[1]} | {key: .name, value: del(.name)}] | from_entries
    }'
}

# Writes $work/latest.json, a lock of the latest (or pinned) versions.
lock_latest() {
  local gateway_api enterprise source version
  gateway_api="${GATEWAY_API_VERSION:-$(gateway_api_latest)}"
  enterprise="${AGENTGATEWAY_ENTERPRISE_VERSION:-$(enterprise_latest)}"
  for source in $SOURCES; do
    version="$gateway_api"
    if [ "$source" = agentgateway-enterprise ]; then version="$enterprise"; fi
    lock_source "$source" "$version" > "$work/$source.json"
  done
  jq -S -n --slurpfile s "$work/gateway-api-standard.json" --slurpfile x "$work/gateway-api-experimental.json" \
    --slurpfile e "$work/agentgateway-enterprise.json" \
    '{sources: {"gateway-api-standard": $s[0], "gateway-api-experimental": $x[0], "agentgateway-enterprise": $e[0]}}' > "$work/latest.json"
}

# Writes $work/compare.json: per source the locked and the latest version and the
# CRDs added, removed and changed between crds.lock.json and $work/latest.json.
compare() {
  [ -f "$LOCK" ] || die "$LOCK does not exist, run: scripts/crds.sh update"
  jq -e '[.sources["gateway-api-standard", "gateway-api-experimental", "agentgateway-enterprise"]]
      | all(.[]; (.version | type) == "string" and (.crds | type) == "object"
          and all(.crds[]; (.kind | type) == "string" and (.versions | type) == "array" and (.sha256 | type) == "string"))' \
    "$LOCK" >/dev/null 2>&1 || die "$LOCK is malformed, restore it or run: scripts/crds.sh update"
  lock_latest
  jq --slurpfile lock "$LOCK" '[.sources | to_entries[] | .key as $source | .value as $new | $lock[0].sources[$source] as $old | {
      source: $source, old: $old.version, new: $new.version,
      names: ($old.crds + $new.crds | keys),
      added: [$new.crds | to_entries[] | select($old.crds[.key] == null) | {name: .key, kind: .value.kind}],
      removed: [$old.crds | to_entries[] | select($new.crds[.key] == null) | {name: .key, kind: .value.kind}],
      changed: [$new.crds | to_entries[] | .key as $name | $old.crds[$name] as $was
        | select($was != null and $was.sha256 != .value.sha256)
        | {name: $name, kind: .value.kind, from: $was.versions, to: .value.versions}]
    }]' "$work/latest.json" > "$work/compare.json"
}

check() {
  local report="" drift=false
  case "${1:-}" in
    "") ;;
    --report) report="${2:?--report needs a file}" ;;
    *) usage >&2; exit 2 ;;
  esac
  compare
  if jq -e 'any(.[]; .added + .removed + .changed | length > 0)' "$work/compare.json" >/dev/null; then drift=true; fi
  {
    if [ "$drift" = true ]; then
      echo "The CRDs that agwmap reads have changed upstream since \`crds.lock.json\` was last updated. Found by \`scripts/crds.sh check\`."
    else
      echo "No CRD that agwmap reads has changed since \`crds.lock.json\` was last updated."
    fi
    echo
    jq -r --arg repo "$GATEWAY_API_REPO" '
      def list($title; note): if length == 0 then empty else "\($title):\n\n" + (map("- `\(.name)` (\(.kind))" + note) | join("\n")) + "\n" end;
      def versions: map("`\(.)`") | join(", ");
      .[] | "## \(.source)\n",
        (if .old == .new then "Locked and latest version: `\(.new)`" else "Locked version: `\(.old)`, latest version: `\(.new)`" end)
        + (if .source | startswith("gateway-api") then " ([release notes](https://github.com/\($repo)/releases/tag/\(.new)))" else "" end) + "\n",
        (.added | list("Added"; "")),
        (.removed | list("Removed"; "")),
        (.changed | list("Changed"; if .from != .to then ", served versions \(.from | versions) -> \(.to | versions)" else "" end)),
        (if .added + .removed + .changed | length > 0 then empty
         elif .old != .new then "The version moved but no CRD changed structurally. This alone is not drift.\n"
         else "No CRD changed.\n" end)' "$work/compare.json"
    if [ "$drift" = true ]; then
      cat <<'EOF'
## What to do

1. Run `scripts/crds.sh diff <crd-name>` (or `scripts/crds.sh diff` for all of them) to see the structural change. It needs curl, jq, yq and helm, and prints to stdout: do not commit its output or paste it here.
2. Check whether the change affects agwmap: a new kind to draw, or a field the map reads that was renamed or removed. Look at `graph.go` (the drawing rules per kind), the "Supported resources" tables in `README.md`, `testdata/` and `examples/`, and change what needs changing.
3. Run `scripts/crds.sh update` and commit `crds.lock.json` together with the fix, or on its own when nothing needs fixing.
EOF
    fi
  } > "$work/report.md"
  if [ -n "$report" ]; then cp "$work/report.md" "$report"; else cat "$work/report.md"; fi
  if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "drift=$drift" >> "$GITHUB_OUTPUT"; fi
  log "drift=$drift"
}

# schema <source> <version> <crd>: the spec as YAML, nothing when that version has no such CRD.
schema() { jq -S --arg name "$3" 'select(.name == $name) | .spec' "$work/$1@$2.jsonl" | yq -p=json -o=yaml; }

crd_diff() {
  local source old new name
  compare
  jq -r --arg name "${1:-}" '.[] | . as $s
    | (if $name == "" then (.added + .removed + .changed)[].name else .names[] | select(. == $name) end)
    | [$s.source, $s.old, $s.new, .] | @tsv' "$work/compare.json" > "$work/diff.tsv"
  if [ ! -s "$work/diff.tsv" ]; then
    if [ -n "${1:-}" ]; then die "no source holds a CRD named $1"; fi
    log "no CRD changed"
  fi
  while IFS=$'\t' read -r source old new name <&3; do
    fetch "$source" "$old"
    schema "$source" "$old" "$name" > "$work/old.yaml"
    schema "$source" "$new" "$name" > "$work/new.yaml"
    if cmp -s "$work/old.yaml" "$work/new.yaml"; then log "$name is the same in $source $old and $new"; continue; fi
    diff -u -L "$name ($source $old)" -L "$name ($source $new)" "$work/old.yaml" "$work/new.yaml" || [ $? -eq 1 ]
  done 3< "$work/diff.tsv"
}

case "${1:-}" in
  check | update | diff) ;;
  *) usage >&2; exit 2 ;;
esac
for tool in curl jq yq helm; do
  command -v "$tool" >/dev/null || die "$tool is not installed"
done
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

case "$1" in
  check) shift; check "$@" ;;
  update) lock_latest; cp "$work/latest.json" "$LOCK"; log "wrote $LOCK" ;;
  diff) crd_diff "${2:-}" ;;
esac
