# AGENTS.md

## Project

Firework is a lightweight pull-based orchestrator for Firecracker microVMs written in Go. 

## Layout

- `cmd/`: contains binary entrypoints.
- `internal/`: contains all the logic, it's a mix of a pure agent logic and control plane logic.
- `docs/`: contains documentation.
- `examples/`: sample agent, control-plane, and node configs.

## Conventions

- Format Go with `gofmt -s` (run `make fmt`).
- Prefer table-driven tests for multi-case tests.
- The scheduler and enricher are pure functions — keep them that way.
- Keep integrations/dependencies (AWS, Git, Firecracker, filesystem, etc) behind interfaces.
- When doing changes, make sure to update docs, examples, and other relevant files.
- Version/commit/build time are injected via ldflags at build time; never hardcode them.

## Building

```bash
make build-agent        # bin/firework-agent
make build-controlplane # bin/firework-controlplane
make build-fc-init      # bin/fc-init (linux/arm64, static)
make build-all          # builds all the binaries
```

## Validation

Be mindful about validating changes. For example, if changes only touching some text files, then there is no need to run full validation on it. At the same time, if unsure, then prefer to run full validation to be on the safe side.

### Unit tests and linters

Use the narrowest useful check first:
- `make fmt`
- `make tidy`
- `make lint`
- `make test`

Unit tests must not require AWS credentials, KVM, or real Firecracker.

### CI validation

Before considering task as done make sure to run the same validations as in CI and make sure that everything is passing. CI logic can be found in `.github/workflows/ci.yaml`.
