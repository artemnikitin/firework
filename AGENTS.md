# AGENTS.md

## Project

Firework is a lightweight pull-based orchestrator for Firecracker microVMs written in Go. 

## Layout

- `internal/`: contains all the logic, it's a mix of a pure agent logic and control plane logic.
- `docs/`: contains documentation.
- `examples/`: sample agent, control-plane, and node configs.

## Conventions

- Format Go with `gofmt -s`
- Prefer table-driven tests.
- Keep scheduler and enricher logic pure where possible.
- Keep integrations (AWS, Git, Firecracker, filesystem, etc) behind interfaces.
- When doing changes, make sure to update docs, examples, and other relevant files.

## Validation

### Unit tests and linters

Use the narrowest useful check first:
- `make test`
- `make test-race`
- `make vet`
- `make lint`

Unit tests must not require AWS credentials, KVM, or real Firecracker.

### CI validation

Before considering task as done make sure to run the same validations as in CI and make sure that everything is passing. CI logic can be found in `.github/workflows/ci.yaml`.

