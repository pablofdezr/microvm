#!/usr/bin/env bash
# Fail if the generated SDK artefacts are out of date with openapi.yaml.
#
# The spec is the source of truth, and three files are generated from it. The
# failure this guards against is subtle and common: someone edits the spec, the
# server and SDKs disagree, and nobody notices until a field is silently
# missing at runtime. Run this in CI so an edit to openapi.yaml that forgot to
# regenerate is a red build, not a production surprise.
#
# It is non-mutating: it saves the current generated files, regenerates, diffs,
# and restores -- so a developer running it locally never has their tree changed
# out from under them.
set -euo pipefail

cd "$(dirname "$0")/.."

GENERATED=(
  "internal/api/apitypes/types.gen.go"
  "sdk/go/microvm/types.gen.go"
  "sdk/typescript/src/types.gen.ts"
)

backup_dir="$(mktemp -d)"
trap 'rm -rf "$backup_dir"' EXIT

# Save the committed versions.
for f in "${GENERATED[@]}"; do
  mkdir -p "$backup_dir/$(dirname "$f")"
  cp "$f" "$backup_dir/$f"
done

# Regenerate in place.
bash api/generate.sh >/dev/null

drift=0
for f in "${GENERATED[@]}"; do
  if ! diff -q "$backup_dir/$f" "$f" >/dev/null; then
    echo "DRIFT: $f is out of date with api/openapi.yaml" >&2
    drift=1
  fi
  # Restore the committed version regardless, so the check leaves no trace.
  cp "$backup_dir/$f" "$f"
done

if [ "$drift" -ne 0 ]; then
  echo "" >&2
  echo "Run 'bash api/generate.sh' and commit the result." >&2
  exit 1
fi
echo "generated SDK artefacts are up to date"
