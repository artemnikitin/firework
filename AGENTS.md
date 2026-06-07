# AGENTS.md

## Project

Firework is a lightweight pull-based orchestrator for Firecracker microVMs written in Go. 

## Layout

- `cmd/`: contains binary entrypoints.
- `internal/`: contains all the logic, it's a mix of a pure agent logic and control plane logic.
- `docs/`: contains documentation.
- `examples/`: sample agent, control-plane, and node configs.

  ## Layout

    - `cmd/agent`: `firework-agent` entry point.
    - `cmd/controlplane`: control-plane entry point.
    - `cmd/fc-init`: guest init process used inside microVM rootfs images.
    - `internal/config`: YAML config types and loading.
    - `internal/enricher`: GitOps input expansion and defaults.
    - `internal/scheduler`: placement and bin-packing logic.
    - `internal/reconciler`: desired-vs-running VM plan/apply logic.
    - `internal/agent`: node runtime.
    - `internal/vm`: VM lifecycle.
    - `internal/network`: host networking.
    - `internal/controlplane`: registry/events/controller runtime.
    - `internal/store`: Git and S3 config backends.
    - `docs/configs/`: source of truth for config formats.
    - `docs/architecture/`: contains the main design/architecture details on the project.
    - `examples/`: sample agent, control-plane, and node configs.

## Conventions

- Format Go with `gofmt -s` (run `make fmt`).
- Prefer table-driven tests for multi-case tests.
- The scheduler and enricher are pure functions — keep them that way.
- Keep integrations/dependencies (AWS, Git, Firecracker, filesystem, etc) behind interfaces.
- Version/commit/build time are injected via ldflags at build time; never hardcode them.
- When changing config schemas, CLI behavior, APIs, or user-visible runtime behavior, update `docs/`, `examples/`, and relevant tests.

## Building

```bash
make build-agent        # bin/firework-agent
make build-controlplane # bin/firework-controlplane
make build-fc-init      # bin/fc-init (linux/arm64, static)
make build-all          # builds all the binaries
```

## Validation

Use the narrowest check that matches the change:

- Docs-only changes: inspect formatting/readability; no Go tests required.
- Go code changes: run `make fmt`, `make test`, and `make lint`.
- Runtime, scheduler, reconciler, store, or control-plane changes: also run `make test-race`.
- Dependency or module changes: run `make tidy` and verify go.mod/go.sum diffs are intentional.
- End-to-end reconcile behavior changes: run `make smoke-local`.

For CI-equivalent local validation, run:
The logic/steps can be found in `.github/workflows/ci.yaml`. 
