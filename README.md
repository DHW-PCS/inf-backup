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
machine run REQUEST_ID snapshots
machine run REQUEST_ID check
machine status REQUEST_ID
```

Machine commands emit one JSON object per line after the literal prefix `INF_BACKUP_EVENT `. Each object contains `protocol`, `request_id`, `operation`, `event`, and `payload`. The optional protocol request ID lets an automated client distinguish a fresh handshake from console history. Terminal events are `succeeded`, `failed`, or `interrupted`; `busy` and `unknown` are also explicit. `machine status` only replays known state and never reruns Restic.

The current request and 32 recent terminal records are written atomically to `/home/container/.inf-backup-protocol.json`. If the wrapper restarts with an active record, that request becomes `interrupted`, because a backup might have reached an uncertain state.

## Safe deployment order

Deploy and verify this wrapper before deploying a maintenance-tool release that requires protocol v1. Verify the human console separately from `machine protocol`.
