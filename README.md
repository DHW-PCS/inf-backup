# inf-backup 3.0.0

`inf-backup` is the Go Restic controller used by the DHW Inf Pterodactyl server. Version 3 replaces the Python 2.0.0 application with one executable. It keeps the default human console and machine protocol v1 used by `inf-tools-go` and the older Python maintenance tools.

## Build and run

Requires Go 1.25 or later to build on Linux or macOS, and Restic >= 0.18.1 at runtime. Rclone is required when the repository uses an rclone backend.

```sh
make build
cp config.yml.example config.yml
# Edit config.yml and create the password file with private permissions.
./bin/inf-backup --config ./config.yml
./bin/inf-backup --version
```

The configuration path may also be a positional argument: `./bin/inf-backup ./config.yml`. Without an argument, the application reads `/home/container/config.yml`. `--help` prints the available startup flags. Standard input is the interactive console; closing it or sending SIGINT/SIGTERM cancels active work and waits for process-group cleanup before exiting.

## Configuration

```yaml
repository: rclone:remote:inf
target_path: /mnt/server
password_file: /home/container/restic-password
tmp_dir: /home/container/tmp
exclude_patterns:
  - /mnt/server/cache/**
```

`repository`, `target_path`, and `password_file` are required. Inline `password` is rejected. `restic_path` defaults to `restic`; `state_path` defaults to `/home/container/.inf-backup-protocol.json`. Repository, password-file and temporary-directory settings are supplied through `RESTIC_REPOSITORY`, `RESTIC_PASSWORD_FILE` and `TMPDIR`. Conflicting inherited inline password, password-command and repository-file variables are removed from the Restic environment. Repository addresses are not printed at startup, and known repository values and URL credentials are redacted from results and errors.

The container bundles the Go executable, Restic, rclone, CA certificates and timezone data. Python and pip are no longer runtime dependencies. The runtime remains pinned to Alpine 3.23.5; the Dockerfile cross-compiles Linux amd64 and arm64 binaries in its build stage.

## Human console

Human-friendly Chinese output is always the default:

```text
backup pre-upgrade
snapshots
check
ls
version
halt
stop
```

`backup` adds a timestamp tag automatically and reports file/byte progress. Tags are always passed as values, including a tag that looks like `--dry-run`. `snapshots` renders a readable inventory. `check` runs the normal metadata consistency check without `--read-data`. `ls [PATH]` lists the first level of a directory, omitting hidden entries and marking directories and symlinks.

`halt` cancels the active task. `stop` cancels active work and exits. Cancellation sends SIGTERM to the full Restic process group, then SIGKILL after a ten-second grace period if necessary, including descendants such as rclone. Human and machine work share one exclusive task slot. Human task output never prints protocol frames, request IDs or raw Restic JSON.

## Machine protocol v1

Automation uses the explicit namespace:

```text
machine protocol [REQUEST_ID]
machine run REQUEST_ID backup TAG...
machine run REQUEST_ID dry-run [TAG...]
machine run REQUEST_ID snapshots
machine run REQUEST_ID check
machine status REQUEST_ID
```

Frames are single lines beginning with `INF_BACKUP_EVENT ` followed by a JSON object containing `protocol`, `request_id`, `operation`, `event`, and `payload`. Valid IDs contain 1–64 ASCII letters, digits, dots, underscores or hyphens and begin with a letter or digit. The optional protocol ID is `null` when omitted.

The handshake includes `wrapper_version: "3.0.0"`, `protocol_version: 1`, the Restic version, `compatible`, and capabilities `backup`, `snapshots`, `check`, `status`, `dry-run`. An incompatible or unavailable Restic reports `compatible: false`; operations are refused before task execution.

Events remain `accepted`, `progress`, `succeeded`, `failed`, `busy`, `interrupted`, and `unknown`. Terminal events are `succeeded`, `failed`, or `interrupted`. Backup success requires exit status zero, exactly one summary and a nonempty `snapshot_id`. Snapshots must return an array of objects. Check requires exit status zero and a unique clean summary. Progress carries file/byte counts and optional current-file information. See the [Restic JSON format](https://restic.readthedocs.io/en/stable/075_scripting.html#json-output).

`dry-run` is machine-only. It runs `restic --json backup --dry-run` with the same target, exclusions, tags, task slot, progress, cancellation and persisted status. Success requires a unique summary with `dry_run: true` and an absent or empty `snapshot_id`. It does not create a verified backup. It still accesses the repository, reads source files and can update local caches and the state journal.

`machine status` only replays state and never executes Restic. Repeating `machine run` with a retained ID replays the known state; reusing that ID for another operation fails. This protection covers the current request and the most recent 32 terminal records, so consumers must still generate fresh random IDs and never blindly resend work after a timeout or lost console.

The journal retains the Python v2 format and is replaced atomically with private file permissions. On restart, an active record becomes `interrupted` with `uncertain: true`. A malformed or unreadable journal prevents startup, preserving recovery evidence. Failed terminal persistence does not turn a verified result into a failure; the result includes `state_persisted: false`. For an interrupted, unknown, timed-out or console-lost backup, inspect snapshots before deciding whether to retry. Output is drained with bounded stderr capture; snapshot documents and individual JSON lines are limited to 32 MiB.

## Using inf-tools-go

Configure the maintenance workspace's `PTERODACTYL_RESTIC_SERVER` with this wrapper's server identifier or UUID, keeping `PTERODACTYL_SERVER` set to Minecraft. The backup server must be running before a restic action:

```sh
# In the inf-tools-go workspace:
./bin/inf-maintenance restic snapshots
./bin/inf-maintenance restic create manual pre-update
./bin/inf-maintenance restic check
```

The CLI and WebUI use the same client. Before backup/save work, it checks wrapper power and the correlated protocol handshake. It pauses Minecraft autosave when needed, restores autosave during cleanup, and independently lists snapshots to verify the reported snapshot ID. JWT recovery sends `machine status REQUEST_ID` and never resends the original operation. A dry-run cannot satisfy a full upgrade's backup requirement.

## Upgrading from 2.0.0

Keep `config.yml`, the password file, source mounts, rclone configuration and `.inf-backup-protocol.json` in their existing locations. The image declares a fixed Docker `ENTRYPOINT`; its script always executes `/usr/local/bin/inf-backup --config /home/container/config.yml`, ignoring command arguments and Pterodactyl's `STARTUP` value entirely. Existing Python startup settings need no panel change and are never expanded, evaluated or printed. For standalone use outside the image, the executable still accepts `--config PATH`. The existing Pterodactyl ready marker remains unchanged.

Deploy the wrapper first and verify its human console and `machine protocol` handshake. Then use the Go maintenance tools with the correct backup server ID. Application version 3.0.0 does not change the wire protocol version. The image workflow publishes `3.0.0`, `3`, `latest`, and commit-SHA tags.

## Development

```sh
make check
make build
docker buildx build --platform linux/amd64,linux/arm64 --output type=cacheonly .
```

`make check` runs formatting, vet, race-enabled offline behavioral tests, shell syntax checks and the cross-project contract when sibling `../inf-tools-go` is present. Set `INF_TOOLS_GO_ROOT=/path/to/inf-tools-go` to require that checkout. Conversely, the maintenance tests discover sibling `../inf-backup`; set `INF_BACKUP_GO_SOURCE=/path/to/inf-backup` to require a particular producer source. The cross-project test builds the Go wrapper, launches a local fake Restic process and passes actual frames through a loopback HTTP/WebSocket panel fixture into the maintenance client. It covers handshake, backup/snapshot verification, clean check, JWT status reconnect and uncertain failures.

For CI contract coverage, set repository variable `INF_TOOLS_GO_REPOSITORY` to the GitHub repository containing the Go maintenance workspace. Without this variable, CI runs the producer's own tests and the cross-project check reports a skip. Tests establish the controller and wire contracts with local fixtures; they do not establish live Restic/backend compatibility. Third-party notices are in `THIRD_PARTY_NOTICES.md`.
