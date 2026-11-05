#!/usr/bin/env bash
# Proof for moving the inline manifests into YAML files: for each of the
# eight, the heredoc its script had at commit 5bc5d6e and the render_yaml
# call the script makes now print the same text, for one fixed set of test
# values. Each new output must also parse as YAML.
#
#   tests/render-matches-heredocs.sh          IDENTICAL or DIFFERENT per manifest
#
# It needs git, GNU envsubst and python3 with PyYAML. It runs no script of the
# repository, writes nothing under .work/ and calls no AWS or Kubernetes API:
# it copies render_yaml and the tag helpers out of lib/common.sh, takes each
# render_yaml call and the lines that compute its values out of the script,
# and stubs aws, kubectl, eksctl and helm so that any call fails.
#
# Only comment lines that a YAML file adds may differ. A YAML file changed on
# purpose after the move makes its manifest DIFFERENT here, by design.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASE="${BASE:-5bc5d6e}"
fail() { printf 'ERROR %s\n' "$*" >&2; exit 2; }
command -v envsubst >/dev/null || fail "envsubst is required"
git -C "$ROOT" cat-file -e "$BASE^{commit}" 2>/dev/null || fail "no commit $BASE in $ROOT"

# name | old script:line where its heredoc starts | new script | new file |
# the values the new script computes for the call
SITES=(
  "db-bootstrap-job|aws/db-bootstrap.sh:93|aws/db-bootstrap.sh|aws/db-bootstrap-job.yaml|"
  "cluster|create-cluster.sh:121|create-cluster.sh|cluster.yaml|PUBLIC_ACCESS_CIDRS CLUSTER_TAGS NODE_TAGS SYSTEM_MAX_NODES GVISOR_MAX_NODES MICROVM_MAX_NODES"
  "storageclass-gp3|create-cluster.sh:243|create-cluster.sh|storageclass-gp3.yaml|"
  "secret-policy|demos/credprovider-aws/deploy.sh:133|demos/credprovider-aws/deploy.sh|demos/credprovider-aws/secret-policy.yaml|"
  "external-secret|demos/db-rotation/deploy.sh:127|demos/db-rotation/deploy.sh|demos/db-rotation/external-secret.yaml|"
  "reloader-config|demos/db-rotation/deploy.sh:177|demos/db-rotation/deploy.sh|demos/db-rotation/reloader-config.yaml|"
  "efs-storage|demos/efs/deploy.sh:165|demos/efs/deploy.sh|demos/efs/storage.yaml|"
  "s3files-storageclass|demos/s3files/deploy.sh:180|demos/s3files/deploy.sh|demos/s3files/storageclass.yaml|"
)

# The test values: fake names, ids and tags, each one different, so that a
# placeholder filled from the wrong variable shows up.
# shellcheck disable=SC2034  # read by name, by render_yaml and the heredocs
test_values() {
  CLUSTER_NAME=talk-test
  AWS_REGION=eu-south-2
  KUBERNETES_VERSION=1.37
  SYSTEM_INSTANCE_TYPE=m8i.large
  GVISOR_INSTANCE_TYPE=m8i.4xlarge
  MICROVM_INSTANCE_TYPE=c8i.2xlarge
  SYSTEM_NODES=3
  GVISOR_NODES=4
  MICROVM_NODES=5
  TAGS="project=agent-substrate team=platform/eks cost-center=cc+42 contact=ops@example.com"
  OWNER_TAG_KEY=agent-substrate-on-eks/deployment
  OWNER_TAG_VALUE=talk-test.eu-south-2
  cidrs='"203.0.113.0/24", "198.51.100.7/32", '
  lt_id=lt-09f8e7d6c5b4a3e21
  lt_version=7
  ACCOUNT=111122223333
  NS=ns-under-test
  IMAGE=public.ecr.aws/docker/library/postgres:18-alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  host=talk-test-pg.c1abcdefgh.eu-south-2.rds.amazonaws.com
  CP_POLICY_CM=cm-under-test
  CP_NS=cp-ns-under-test
  CP_ATESPACE=atespace-granted
  CP_DENIED_ATESPACE=atespace-denied
  SECRET_PREFIX=secret-prefix-under-test
  SVC=svc-under-test
  SC=sc-under-test
  fs="fs-0a1b2c3d4e5f6a7b8"
  EFS_UID=1001
  EFS_GID=1002
}
stubs() {
  aws()     { echo "stub: aws $*" >&2; return 99; }
  kubectl() { echo "stub: kubectl $*" >&2; return 99; }
  eksctl()  { echo "stub: eksctl $*" >&2; return 99; }
  helm()    { echo "stub: helm $*" >&2; return 99; }
}

# fn_src NAME: the definition of shell function NAME in the file on stdin.
fn_src() {
  awk -v n="$1" '
    !on && $0 ~ ("^" n "\\(\\) *\\{") { on = 1; print; if ($0 ~ /\}[[:space:]]*$/) exit; next }
    on { print; if ($0 ~ /^\}/) exit }'
}
OLD_LIB=$(git -C "$ROOT" show "$BASE:lib/common.sh")
NEW_LIB=$(cat "$ROOT/lib/common.sh")
OLD_FNS=""; NEW_FNS=""
for f in _tag_pairs tags_yaml tags_kv; do
  OLD_FNS+="$(fn_src "$f" <<<"$OLD_LIB")"$'\n'
done
for f in die _tag_pairs tags_yaml tags_kv render_yaml; do
  NEW_FNS+="$(fn_src "$f" <<<"$NEW_LIB")"$'\n'
done
grep -q '^render_yaml()' <<<"$NEW_FNS" || fail "no render_yaml in lib/common.sh"

# old_output SCRIPT LINE: what the heredoc starting at LINE of SCRIPT (at
# BASE) printed, with the test values.
old_output() {
  local src start body
  src=$(git -C "$ROOT" show "$BASE:$1")
  start=$(sed -n "${2}p" <<<"$src")
  [[ "$start" =~ \<\<(\'?)([A-Z]+)\'? ]] || fail "$1:$2 starts no heredoc: $start"
  local q="${BASH_REMATCH[1]}" delim="${BASH_REMATCH[2]}"
  body=$(awk -v s="$2" -v d="$delim" 'NR > s { if ($0 == d) { found = 1; exit } print }
    END { if (!found) exit 1 }' <<<"$src") || fail "$1:$2: no closing $delim"
  (
    stubs; test_values
    eval "$OLD_FNS"
    eval "cat <<$q$delim$q"$'\n'"$body"$'\n'"$delim"
  )
}

# new_output SCRIPT FILE PREP: the output of the script's render_yaml call
# for FILE, with the test values and the script's own lines for PREP.
new_output() {
  local script="$1" file="$2" prep="$3" joined calls words v line
  # Join continued lines, then find the call for FILE: render_yaml, the
  # file, and the variable names after it.
  joined=$(sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$ROOT/$script")
  calls=$(grep -F "render_yaml \"\$ROOT/$file\"" <<<"$joined" || true)
  [ "$(grep -c . <<<"$calls")" = 1 ] || fail "$script: want one render_yaml call for $file, found: $calls"
  read -ra words <<<"${calls#*render_yaml }"
  # The names end at the first word that is not one (|, >), or after a name
  # that closes the command substitution, as in NAME).
  local names=()
  for v in "${words[@]:1}"; do
    [[ "$v" =~ ^([A-Za-z_][A-Za-z0-9_]*)(\)*)$ ]] || break
    names+=("${BASH_REMATCH[1]}")
    [ -z "${BASH_REMATCH[2]}" ] || break
  done
  (
    stubs; test_values
    eval "$NEW_FNS"
    for v in $prep; do
      line=$(grep -E "^[[:space:]]*$v=" "$ROOT/$script") || fail "$script: no line sets $v"
      [ "$(grep -c . <<<"$line")" = 1 ] || fail "$script: more than one line sets $v"
      eval "$line"
    done
    render_yaml "$ROOT/$file" ${names[@]+"${names[@]}"}
  )
}

# compare OLD NEW: 0 for the same bytes, 1 when NEW only adds comment lines,
# 2 for any other difference (printed as a unified diff).
compare() {
  python3 - "$1" "$2" <<'PY'
import difflib, re, sys
old = open(sys.argv[1], "rb").read()
new = open(sys.argv[2], "rb").read()
if old == new:
    sys.exit(0)
a, b = old.decode().splitlines(True), new.decode().splitlines(True)
ok = True
for op, i1, i2, j1, j2 in difflib.SequenceMatcher(None, a, b, autojunk=False).get_opcodes():
    if op == "equal":
        continue
    if op == "insert" and all(re.match(r"^\s*#", l) for l in b[j1:j2]):
        continue
    ok = False
if ok:
    sys.exit(1)
sys.stdout.writelines(difflib.unified_diff(a, b, "old heredoc", "new render_yaml"))
sys.exit(2)
PY
}

# KEEP=DIR keeps both outputs of each manifest in DIR (NAME.old, NAME.new).
if [ -n "${KEEP:-}" ]; then
  tmp="$KEEP"; mkdir -p "$tmp"
else
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
fi
bad=0
for site in "${SITES[@]}"; do
  IFS='|' read -r id old new file prep <<<"$site"
  old_output "${old%%:*}" "${old##*:}" > "$tmp/$id.old"
  new_output "$new" "$file" "$prep" > "$tmp/$id.new"
  docs=$(python3 -c 'import sys, yaml; print(sum(1 for d in yaml.safe_load_all(open(sys.argv[1])) if d is not None))' "$tmp/$id.new" 2>&1) \
    || { docs="does not parse: ${docs##*$'\n'}"; bad=1; }
  sum=$(python3 -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest()[:12])' "$tmp/$id.new")
  st=0; out=$(compare "$tmp/$id.old" "$tmp/$id.new") || st=$?
  case "$st" in
    0) printf 'IDENTICAL  %-21s %s:%s -> %s (%s lines, sha256 %s, %s YAML docs)\n' \
         "$id" "${old%%:*}" "${old##*:}" "$file" "$(wc -l < "$tmp/$id.new")" "$sum" "$docs" ;;
    1) printf 'IDENTICAL  %-21s %s:%s -> %s (apart from added comment lines; %s YAML docs)\n' \
         "$id" "${old%%:*}" "${old##*:}" "$file" "$docs" ;;
    *) printf 'DIFFERENT  %-21s %s:%s -> %s\n%s\n' "$id" "${old%%:*}" "${old##*:}" "$file" "$out"; bad=1 ;;
  esac
done
if [ "$bad" = 0 ]; then
  echo "all ${#SITES[@]} manifests render as their heredocs did at $BASE"
else
  echo "FAILED"
  exit 1
fi
