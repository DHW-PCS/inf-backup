# Agent Guide: inf-backup (Go 3)

This file is the execution contract for agents working in this repository. `inf-backup` runs Restic inside a Pterodactyl container. Preserve the human console as the permanent default interface and `machine` as the only automation namespace.

## Working posture

Before changing code, inspect `cmd/inf-backup`, the relevant `internal` packages and tests, `README.md`, `Dockerfile`, `entrypoint-posix.sh`, and `.github/workflows/build.yml`. Run `git status --short --branch` and preserve user-owned modifications and untracked files. Identify effects on human rendering, framing, task exclusion, recovery state, process control and packaging. Add focused offline behavioral tests for those changes and verify both interfaces when their shared controller changes.

Base claims on current source, offline tests or explicitly approved isolated local Restic validation. Mocked tests alone do not establish that a Restic command works; state unverified runtime behavior instead of requiring a container smoke test.

## Authority and secrets

- Repository work and routine validation never authorize deployment, live panel commands, production backup creation, snapshots or check.
- Keep tests offline. Use process fixtures or explicitly approved temporary local Restic repositories. Never use production repositories, password files, rclone remotes or mounted server data.
- Never print or commit passwords, repository credentials, private keys, panel credentials or live protocol-state data. Do not read passwords for logging. Sanitize diagnostic messages before output and persistence.
- Leave `container/`, including its data, configuration and credentials, untouched. Do not introduce smoke tests or validation commands that mount or copy it into a simulated container. Container smoke tests are not required.

## Interface contracts

- Human commands remain `backup`, `snapshots`, `check`, `ls`, `version`, `halt`, and `stop`, with readable Chinese output. Never emit machine frames, request IDs or raw Restic JSON from human tasks.
- Preserve file/byte progress, automatic timestamp tags, readable snapshot summaries and actionable guidance. Ordinary check never adds `--read-data`. Human tags cannot become Restic flags; dry-run is machine-only.
- Protocol v1 frames contain `protocol`, `request_id`, `operation`, `event`, and `payload` on one line after `INF_BACKUP_EVENT `. Events are only `accepted`, `progress`, `succeeded`, `failed`, `busy`, `interrupted`, and `unknown`.
- Validate IDs and operations before execution. Status and retained duplicate run requests only replay state. Never retry unknown, interrupted, timed-out or console-lost work automatically.
- Backup requires zero exit, exactly one summary and a nonempty snapshot ID. Snapshots requires an object array. Check requires zero exit and a clean summary.
- `machine run ID dry-run [TAG...]` maps to `restic --json backup --dry-run` and advertises `dry-run`. It shares target, exclusions, tags, task slot, progress, cancellation and state. Success requires zero exit, a unique `dry_run: true` summary and an absent or empty snapshot ID. It cannot establish backup creation.
- Preserve compatibility with `inf-tools-go` and legacy Python consumers. Run the cross-project test after framing or recovery changes.

## Task, recovery and process safety

- Human and machine tasks share one exclusive controller slot. No parallel Restic execution path.
- Atomically store the active request and most recent 32 terminal records with private permissions. Preserve the Python v2 journal schema. Recovered active requests become uncertain `interrupted` records. Do not overwrite malformed recovery evidence.
- Never downgrade verified success because terminal persistence failed; expose `state_persisted: false`. Do not release the task slot until output, persistence and process cleanup finish.
- Launch Restic using argument arrays without a shell and in a new process session. Halt, stop, EOF and SIGINT/SIGTERM terminate the complete process group, escalating to SIGKILL after the bounded grace period, including descendants that outlive the leader.
- Use `RESTIC_REPOSITORY`, `RESTIC_PASSWORD_FILE`, and `TMPDIR`. Reject inline config passwords and remove conflicting inherited password/repository sources.
- Preserve Restic >= 0.18.1, pinned Alpine 3.23.5 and the Pterodactyl ready marker. Go 3 intentionally removes Python and PyYAML runtime dependencies.
- The container entrypoint must ignore Pterodactyl's `STARTUP` and directly exec `/usr/local/bin/inf-backup --config /home/container/config.yml`. Never evaluate, expand or print the panel startup value.

## Repository map

| Path | Ownership |
| --- | --- |
| `cmd/inf-backup` | Flags, signals, stdin loop and shutdown |
| `internal/config` | YAML validation, environment, tags and credential redaction |
| `internal/console` | Shared controller, human rendering, command routing and protocol output |
| `internal/protocol` | Version constants and event framing |
| `internal/restic` | Session/process-group control, progress and strict JSON validation |
| `internal/state` | Atomic compatible recovery journal |
| `scripts/consumer-contract.sh` | Offline contract with sibling inf-tools-go |
| `Dockerfile`, `entrypoint-posix.sh`, `.github/workflows/build.yml` | Container and multi-architecture packaging |

## Validation and documentation

Run `make check`, `make build`, and `git diff --check`. Tests use only temporary local files and fake Restic subprocesses. The cross-project check is automatic when sibling inf-tools-go exists; an explicit `INF_TOOLS_GO_ROOT` requires it. A consumer contract change must update `internal/workflow/backup_go_contract_test.go` in that workspace as well.

When Docker changes, validate both architectures without starting a simulated server:

```sh
docker buildx build --platform linux/amd64,linux/arm64 --output type=cacheonly .
```

Keep README, config example, flags, protocol fields, versions, startup instructions and this guide synchronized. Keep agent-only authority restrictions here. Never commit binaries, caches, temporary repositories, journals or secrets.
