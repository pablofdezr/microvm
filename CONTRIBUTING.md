# Contributing

Thanks for your interest in microvm.

## Layout

- The daemon (`microvmd`), CLI (`microvm`) and guest agent are Go — see `go.mod`
  (Go 1.26+). Host-side code lives under `cmd/` and `internal/`.
- The SDKs live under `sdk/` (Go, Python, TypeScript). They are generated from
  the OpenAPI spec in `api/` — **the spec is the source of truth**, so regenerate
  the `types.gen.*` files rather than hand-editing them.

## Building and testing

```sh
go build ./...
go vet ./...
go test ./...
```

The end-to-end tests under `test/e2e` need root, KVM, and Firecracker on `PATH`;
they are skipped otherwise. See the "Testing" and "Building images" sections of
the [README](README.md) for the full workflow.

## Pull requests

- Keep changes focused and explain the *why* in the description.
- Run `go test ./...` and `go vet ./...` before opening a PR.
- Add or update tests for behavior changes.

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
