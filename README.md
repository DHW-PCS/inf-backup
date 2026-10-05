# inf-backup

`inf-backup` is the Restic controller used by the DHW Inf Pterodactyl server. It has two first-class interfaces:

- a human-friendly interactive console, which is the default;
- an explicit versioned machine protocol used by `inf_maintenance_tools`.

## Configuration

The wrapper reads `/home/container/config.yml` by default:

```yaml
repository: rclone:remote:inf
target_path: /mnt/server
password_file: /home/container/restic-password
tmp_dir: /home/container/tmp
exclude_patterns:
  - /mnt/server/cache/**
```

`repository`, `target_path`, and `password_file` are required. Inline passwords are rejected. `restic_path` and `state_path` may override the Restic executable and protocol-state file. Repository and password settings are passed to Restic through `RESTIC_REPOSITORY` and `RESTIC_PASSWORD_FILE`.

## Human console

Human-friendly output is always the default:

```text
backup pre-upgrade
snapshots
check
ls
version
halt
stop
```

`backup` adds a timestamp tag automatically and reports file/byte progress. `snapshots` renders a readable inventory. `check` runs the normal metadata consistency check; it does not add `--read-data`. `halt` and `stop` terminate the full active Restic process group. Only one human or machine task may run at a time.

Human commands never print machine protocol frames or raw Restic JSON.

## Machine protocol v1

Automation must use the explicit namespace:

```text
machine protocol [REQUEST_ID]
machine run REQUEST_ID backup TAG...
machine run REQUEST_ID dry-run [TAG...]
machine run REQUEST_ID snapshots
machine run REQUEST_ID check
machine status REQUEST_ID
```

Machine commands emit one JSON object per line after the literal prefix `INF_BACKUP_EVENT `. Each object contains `protocol`, `request_id`, `operation`, `event`, and `payload`. The optional protocol request ID lets an automated client distinguish a fresh handshake from console history. Terminal events are `succeeded`, `failed`, or `interrupted`; `busy` and `unknown` are also explicit. `machine status` only replays known state and never reruns Restic.

`dry-run` is available only through the machine interface and is advertised in the handshake's `capabilities`. It runs `restic --json backup --dry-run` with the configured target, exclusions, supplied tags, and automatic timestamp tag. It shares the exclusive task slot, progress events, cancellation, and persisted status with real backups. All its frames use `operation: "dry-run"`; success requires exit status zero and exactly one summary with `dry_run: true` and no nonempty `snapshot_id`. The success payload contains Restic's summary and estimates, not a created snapshot. Dry-run still needs repository access and reads source files; it may update local caches and the wrapper state journal, so it is not a side-effect-free configuration check. No additional container configuration is required.

Protocol v1 and the existing capabilities remain unchanged apart from this additive capability. Consumers must check for `dry-run` before requesting it. A dry-run is not a backup and must never satisfy a requirement for a verified backup. For an interrupted, unknown, timed-out, or console-lost real backup, inspect snapshots before deciding whether to retry; `machine status` does not retry it.

The current request and 32 recent terminal records are written atomically to `/home/container/.inf-backup-protocol.json`. If the wrapper restarts with an active record, that request becomes `interrupted`, because a backup might have reached an uncertain state.

## Safe deployment order

Deploy and verify this wrapper before deploying a maintenance-tool release that requires protocol v1. Verify the human console separately from `machine protocol`.
