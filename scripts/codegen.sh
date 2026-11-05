#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

mode="${1:-generate}"
case "$mode" in
  generate|check) ;;
  *) echo "Usage: scripts/codegen.sh [generate|check]" >&2; exit 2 ;;
esac

if ! mkdir .codegen; then
  echo "Remove or finish the existing .codegen staging directory before regenerating." >&2
  exit 1
fi
trap 'rm -rf .codegen' EXIT

if [[ "$(yq '.sources | length' tfspecgen.yaml)" != 1 ]]; then
  echo "This pipeline requires exactly one OpenAPI 3 source." >&2
  exit 1
fi
spec="$(yq -r '.sources[0].spec.path' tfspecgen.yaml)"
safelist="$(yq -r '.sources[0].safelist' tfspecgen.yaml)"
package="$(yq -r '.sources[0].package' tfspecgen.yaml)"
config="$(yq -r '.sources[0].ogen_config' tfspecgen.yaml)"
source_name="$(yq -r '.sources[0].name' tfspecgen.yaml)"
if [[ "$(yq -r '.sources[0].spec.format' tfspecgen.yaml)" != openapi3 ]]; then
  echo "This pipeline requires an OpenAPI 3 contract." >&2
  exit 1
fi
expected_ogen="v$(yq -r '.tools."go:github.com/ogen-go/ogen/cmd/ogen"' mise.toml)"
actual_ogen="$(go version -m "$(command -v ogen)" | awk '$1 == "mod" { print $3; exit }')"
if [[ "$actual_ogen" != "$expected_ogen" ]]; then
  echo "ogen must be $expected_ogen; found $actual_ogen." >&2
  exit 1
fi

# WHY: The public ogen check must use the same paths as tfspecgen's private filter.
# This small source contains only safelisted operations; expanding it needs a
# public filtering stage before direct generation can remain equivalent.
yq -r '.paths | keys | .[]' "$spec" | LC_ALL=C sort > .codegen/paths
LC_ALL=C sort "$safelist" > .codegen/safelist
diff -u .codegen/safelist .codegen/paths

snapshot() {
  shasum -a 256 .tfspecgen-version mise.toml tfspecgen.yaml "$spec" "$safelist" "$config" \
    internal/"$package"/*.go generated/"${source_name}_ogen_introspection.json"
}

if [[ "$mode" == generate ]]; then
  tool="$(command -v tfspecgen)"
  expected="$(cat .tfspecgen-version)"
  actual="$(go version -m "$tool" | awk '$1 == "mod" { print $3; exit }')"
  if [[ "$actual" != "$expected" ]]; then
    echo "tfspecgen must be $expected; found $actual." >&2
    exit 1
  fi
  tfspecgen filter -spec "$spec" -safelist "$safelist" -out .codegen/filtered.yaml
  tfspecgen client -spec .codegen/filtered.yaml -target "internal/$package" -package "$package" -config "$config"
  cp -R "internal/$package" ".codegen/$package"
  # HACK: Pinned tfspecgen requires this file even when ogen emits no parameters.
  # The staging-only empty AST declares no invented API parameters. Remove this
  # workaround when tfspecgen accepts absent oas_parameters_gen.go.
  if [[ ! -f ".codegen/$package/oas_parameters_gen.go" ]]; then
    printf 'package %s\n' "$package" > ".codegen/$package/oas_parameters_gen.go"
  fi
  tfspecgen introspect -src ".codegen/$package" -out "generated/${source_name}_ogen_introspection.json"
  tfspecgen generate -config tfspecgen.yaml -intro "generated/${source_name}_ogen_introspection.json" -out .codegen/resources
  snapshot > "generated/${source_name}_codegen.sha256"
else
  ogen --target ".codegen/$package" --package "$package" --clean --config "$config" "$spec"
  diff -ru "internal/$package" ".codegen/$package"
  # NOTE: Public CI verifies the client and the last full regeneration's inputs
  # and artifacts. It cannot execute the private tfspecgen introspector.
  snapshot > .codegen/snapshot.sha256
  diff -u "generated/${source_name}_codegen.sha256" .codegen/snapshot.sha256
fi
