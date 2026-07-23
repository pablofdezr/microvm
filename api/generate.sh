#!/usr/bin/env bash
# Regenerate everything the OpenAPI spec is the source of truth for.
#
# Run this after editing openapi.yaml. Never edit a *.gen.* file: the next run
# of this script overwrites it, so a hand-edit there is a change that silently
# disappears.
#
# Three artefacts come out of one spec. That is not duplication -- there is a
# single place a field is declared, and nobody keeps two copies in step by hand.
# It is how Stripe does it, and for the same reason: the server and an SDK are
# separate programs that must agree, and the only durable way to make two
# programs agree is to generate both from one description.
set -euo pipefail

cd "$(dirname "$0")/.."
SPEC="api/openapi.yaml"

# Pinned. An unpinned generator turns "regenerate" into "roll the dice on a
# toolchain update", and the diff lands in code nobody reads.
OAPI_CODEGEN="github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.2"
OPENAPI_TS="openapi-typescript@7.13.0"
REDOCLY="@redocly/cli@2.5.0"

say() { printf '\033[1m%s\033[0m\n' "$*"; }

say "==> validating $SPEC"
# Validate first. Every generator below will happily produce plausible garbage
# from a spec that is subtly wrong, and finding that out in the generated code
# is finding out in the worst possible place.
npx --yes "$REDOCLY" lint "$SPEC"

say "==> Go: server wire types"
mkdir -p internal/api/apitypes
go run "$OAPI_CODEGEN" -config api/codegen/server.yaml "$SPEC"

say "==> Go: SDK types"
mkdir -p sdk/go/microvm
go run "$OAPI_CODEGEN" -config api/codegen/sdk-go.yaml "$SPEC"

say "==> TypeScript: SDK types"
mkdir -p sdk/typescript/src
# --default-non-nullable=false: without it, a property with a `default` is
# emitted as *required*. That is backwards for a request type -- a default
# exists precisely so a caller can leave the field out -- and it would force
# every caller to pass a timeout they do not care about.
npx --yes "$OPENAPI_TS" "$SPEC" --default-non-nullable=false -o sdk/typescript/src/types.gen.ts

say "==> gofmt"
gofmt -w internal/api/apitypes sdk/go/microvm

say "==> done"
echo "  internal/api/apitypes/types.gen.go   $(wc -l < internal/api/apitypes/types.gen.go) lines"
echo "  sdk/go/microvm/types.gen.go          $(wc -l < sdk/go/microvm/types.gen.go) lines"
echo "  sdk/typescript/src/types.gen.ts      $(wc -l < sdk/typescript/src/types.gen.ts) lines"
