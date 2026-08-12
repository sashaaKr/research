#!/usr/bin/env bash
# Proves the Go service and this Rust port render byte-identical manifests.
#
# Without this the benchmark is meaningless: two programs doing different
# amounts of work produce two numbers that cannot be compared. Every config
# shape below must report "differing: 0".
set -euo pipefail

GO_DIR="${GO_DIR:-../golang-hedp}"
GO_LIB="${GO_LIB:-$GO_DIR/testdata/library}"
RS_LIB="${RS_LIB:-$GO_DIR/testdata/library-jinja}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cargo build --release --quiet
(cd "$GO_DIR" && go build -o "$TMP/dumpgo" ./cmd/dumpmanifests)

cat > "$TMP/diff.py" <<'PY'
import json, sys, difflib
go, rs = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
gk, rk = set(go), set(rs)
shared = gk & rk
diff = [k for k in sorted(shared) if go[k] != rs[k]]
print(f"keys go={len(gk)} rust={len(rk)} shared={len(shared)} differing={len(diff)}")
if gk - rk: print("  only in go:  ", sorted(gk - rk)[:5])
if rk - gk: print("  only in rust:", sorted(rk - gk)[:5])
for k in diff[:1]:
    print(f"  --- {k} ---")
    print("\n".join(list(difflib.unified_diff(
        go[k].splitlines(), rs[k].splitlines(), "go", "rust", lineterm="", n=1))[:30]))
sys.exit(1 if (diff or gk != rk) else 0)
PY

fail=0
for spec in "minimal:" "typical:observability,mesh" "heavy:observability,mesh,search,analytics,cdn" "all:"; do
  name="${spec%%:*}"; feats="${spec#*:}"
  extra=(); [ "$name" = "all" ] && extra=(--all)
  goextra=(); [ "$name" = "all" ] && goextra=(-all)

  "$TMP/dumpgo" -library "$GO_LIB" -features "$feats" "${goextra[@]}" > "$TMP/go.json" 2>/dev/null
  ./target/release/dump --library "$RS_LIB" --features "$feats" "${extra[@]}" > "$TMP/rs.json" 2>/dev/null

  printf '%-9s ' "$name"
  python3 "$TMP/diff.py" "$TMP/go.json" "$TMP/rs.json" || fail=1
done

if [ "$fail" -ne 0 ]; then
  echo "FAIL: the two engines do not render the same bytes; benchmarks are not comparable"
  exit 1
fi
echo "OK: byte-identical across every config shape"
