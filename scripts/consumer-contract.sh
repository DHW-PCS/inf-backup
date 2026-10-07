#!/bin/sh
set -eu

BACKUP_SOURCE=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
CONSUMER_SOURCE=${INF_TOOLS_GO_ROOT:-"$BACKUP_SOURCE/../inf-tools-go"}
if [ ! -f "$CONSUMER_SOURCE/go.mod" ]; then
    if [ -n "${INF_TOOLS_GO_ROOT:-}" ]; then
        printf '%s\n' 'Configured inf-tools-go source is unavailable.' >&2
        exit 1
    fi
    printf '%s\n' 'Sibling inf-tools-go unavailable; cross-project contract skipped. Set INF_TOOLS_GO_ROOT to require it.'
    exit 0
fi
CONSUMER_SOURCE=$(CDPATH= cd -- "$CONSUMER_SOURCE" && pwd)
cd "$CONSUMER_SOURCE"
INF_BACKUP_GO_SOURCE="$BACKUP_SOURCE" go test -race ./internal/workflow -run '^TestGoBackupConsumerContract$' -count=1
