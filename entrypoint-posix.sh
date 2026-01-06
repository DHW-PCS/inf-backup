#!/bin/sh

# Default the TZ environment variable to UTC.
: "${TZ:=UTC}"
export TZ

# Set environment variable that holds the Internal Docker IP
INTERNAL_IP=$(ip route get 1 2>/dev/null | awk '{print $(NF-2); exit}')
export INTERNAL_IP

# Switch to the container's working directory
cd /home/container || exit 1

# Convert all of the "{{VARIABLE}}" parts of the command into the expected shell
# variable format of "${VARIABLE}" before evaluating the string and automatically
# replacing the values.
if [ -n "${STARTUP:-}" ]; then
    PARSED=$(printf '%s' "$STARTUP" | sed -e 's/{{/${/g' -e 's/}}/}/g')
    # Expand variables while preserving whitespace
    PARSED=$(eval "echo \"$PARSED\"")
else
    PARSED=""
fi

# Display the command we're running, and then execute it with the env from the container itself.
printf '\033[1m\033[33mcontainer@pterodactyl~ \033[0m%s\n' "$PARSED"
# shellcheck disable=SC2086
exec env $PARSED
