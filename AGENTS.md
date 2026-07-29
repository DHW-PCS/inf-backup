# Agent Guide: inf-backup

This file is the execution contract for agents working in this repository.

`inf-backup` runs Restic inside a Pterodactyl container. Preserve the human console as the permanent default interface and the `machine` namespace as the only automation protocol.

## Working Posture

Before changing code:

1. Inspect `app/app.py`, focused tests, `README.md`, `Dockerfile`, `entrypoint-posix.sh`, and `.github/workflows/build.yml`.
2. Run `git status --short --branch`; preserve all user-owned modifications and untracked files.
3. Identify whether the change affects human rendering, protocol framing, task exclusion, persisted recovery state, Restic process control, or container packaging.
4. Add focused offline tests with implementation changes and verify both interfaces when their shared controller changes.

Base claims on current source, tests, or an approved local-container smoke test. Mocked tests alone do not establish that a Restic command works.

## Authority and Secrets

- Repository work and routine validation do not authorize deployment, live Pterodactyl commands, production backup creation, production `snapshots`, or production `check`.
- Keep tests offline and use only temporary local Restic repositories. Never use a production repository, password file, rclone remote, or mounted server data.
- Never commit or print Restic passwords, repository credentials, private keys, Pterodactyl credentials, or protocol-state data from a live installation.
- Deployment and any live Restic operation require separate explicit approval.

## Interface Contracts

### Human Console

- `backup`, `snapshots`, `check`, `ls`, `version`, `halt`, and `stop` remain human-facing commands with readable text output.
- Human commands must never emit `INF_BACKUP_EVENT`, request IDs, Python representations, or raw Restic JSON.
- Keep `backup` file/byte progress, automatic timestamp tags, readable snapshot summaries, and actionable Chinese error guidance.
- `check` remains the ordinary metadata check; do not add `--read-data` by default.

### Machine Protocol

- Structured output is available only under `machine`: `protocol`, `run`, and `status`.
- Every frame is one line beginning with `INF_BACKUP_EVENT ` followed by a JSON object containing `protocol`, `request_id`, `operation`, `event`, and `payload`.
- Protocol v1 events are limited to `accepted`, `progress`, `succeeded`, `failed`, `busy`, `interrupted`, and `unknown`.
- Validate request IDs and operations before execution. `machine status REQUEST_ID` only replays state and must never execute or retry Restic.
- Preserve strict Restic output validation: backup requires one valid summary and `snapshot_id`; snapshots requires an array; check requires exit status zero and a clean summary.
- Keep protocol changes coordinated with `inf_maintenance_tools`; add or update the cross-repository consumer test when the frame contract changes.

### Task and Recovery Safety

- Human and machine work share one controller and one exclusive task slot. Never create a bypass lock or parallel Restic execution path.
- Store the active request and the most recent 32 terminal records atomically. On startup, convert a persisted active request to `interrupted` with an uncertain result.
- Never blindly retry an interrupted, unknown, timed-out, or console-lost backup; direct the caller to inspect snapshots first.
- Start Restic in a new process session. `halt` and `stop` must terminate the complete process group, escalating from `SIGTERM` to `SIGKILL` after the bounded grace period.

## Restic and Container Contracts

- Invoke Restic with argument arrays and `shell=False`; do not construct shell command strings.
- Pass repository, password file, and temporary-directory settings through `RESTIC_REPOSITORY`, `RESTIC_PASSWORD_FILE`, and `TMPDIR`. Reject inline passwords.
- Preserve the minimum Restic version check (`>=0.18.1`), the pinned Alpine patch release, and the pinned PyYAML dependency unless a tested upgrade intentionally changes them.
- Keep the Pterodactyl startup-ready marker printed by `app/app.py`.

## Repository Map

| Path | Ownership |
| --- | --- |
| `app/app.py` | Configuration, human rendering, machine protocol, task state, Restic execution, and process control |
| `app/requirements.txt` | Pinned Python runtime dependencies |
| `tests/test_app.py` | Offline controller, protocol, parsing, recovery, and process-group tests |
| `tests/container_smoke.py` | Temporary local Restic repository smoke test for human and machine interfaces |
| `Dockerfile` / `entrypoint-posix.sh` | Runtime image and Pterodactyl container entrypoint |
| `.github/workflows/build.yml` | Offline tests, container smoke test, and multi-architecture image build |
| `README.md` | Human-facing configuration, console, protocol, recovery, and deployment documentation |

## Validation

Use Python 3.13 for repository tests:

```sh
python -m pip install -r app/requirements.txt
python -m unittest discover -s tests -p 'test_*.py' -v
python -m compileall -q app tests
python -m black --check --line-length 100 --skip-string-normalization --skip-magic-trailing-comma app tests
git diff --check
```

When Restic execution, the Docker image, or either public interface changes, also run the local-container smoke test:

```sh
docker build -t inf-backup-smoke:local .
python tests/container_smoke.py inf-backup-smoke:local
docker buildx build --platform linux/amd64,linux/arm64 --output type=cacheonly .
```

The smoke test must use its generated Docker volume and temporary local repository, then remove them even on failure.

## Documentation and Hygiene

- When configuration, commands, protocol fields, versions, deployment order, or recovery behavior changes, update `README.md`, tests, container configuration, and this guide together.
- Keep AI-only execution restrictions in this file and keep `README.md` focused on maintainers and protocol consumers.
- Do not commit `.venv`, bytecode, caches, local images, temporary repositories, state journals, or secrets.
