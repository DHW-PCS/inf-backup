#!/bin/sh

# Default the TZ environment variable to UTC.
: "${TZ:=UTC}"
export TZ

# Set environment variable that holds the Internal Docker IP
INTERNAL_IP=$(ip route get 1 2>/dev/null | awk '{print $(NF-2); exit}')
export INTERNAL_IP

# Switch to the container's working directory
cd /home/container || exit 1

# The image owns startup. Pterodactyl's STARTUP setting is intentionally ignored.
printf '\033[1m\033[33mcontainer@pterodactyl~ \033[0mStarting inf-backup\n'
exec /usr/local/bin/inf-backup --config /home/container/config.yml
