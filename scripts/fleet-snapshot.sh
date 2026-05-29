#!/usr/bin/env bash
# scripts/fleet-snapshot.sh
#
# Take a runtime.db snapshot inside a running chassis container and
# publish it to the configured artifact store (TXCO_ARTIFACT_STORE).
# Designed to be run from cron / systemd-timer, or on-demand before
# bringing up a new fleet member.
#
# By default ALSO writes a `snapshots/latest` alias pointing at the
# same bytes — a new chassis can boot with
# `TXCO_SNAPSHOT_BOOTSTRAP_REF=snapshots/latest` and always get the
# freshest snapshot without re-configuring per snapshot cadence.
#
# Usage:
#   sudo ./scripts/fleet-snapshot.sh                          # default
#   sudo CONTAINER=foo ./scripts/fleet-snapshot.sh            # different container
#   sudo NO_ALIAS=1 ./scripts/fleet-snapshot.sh               # skip the alias
#   sudo ALIAS_KEY=snapshots/prod ./scripts/fleet-snapshot.sh # custom alias
#
# Exit status:
#   0  snapshot published; the artifact key is the LAST line of stdout
#   1  failure (chassis container missing, publish errored, etc.)
#
# Operational notes:
# - The chassis runs as uid 1000; `docker exec` runs as the same
#   uid by default, so the snapshot reads runtime.db with the
#   chassis's own credentials. No host-side sqlite3 or aws CLI needed.
# - R2 / S3 / file backend choice is whatever the running chassis
#   was configured with — the publish subcommand inherits the
#   container's environment, including /data/secrets/txco/.env.
# - Snapshot keys are timestamp-sortable: a prefix list (e.g.
#   `aws s3 ls`) is automatically newest-last. Use them to GC old
#   snapshots on a separate cadence if storage is a concern.

set -euo pipefail

CONTAINER="${CONTAINER:-txco-txco-1}"
DB_PATH_IN_CONTAINER="${DB_PATH_IN_CONTAINER:-/data/db/runtime-prod.db}"
ALIAS_KEY="${ALIAS_KEY:-snapshots/latest}"

# Verify the chassis container exists + is running.
if ! docker inspect -f '{{.State.Running}}' "${CONTAINER}" 2>/dev/null | grep -q true; then
    echo "FAIL: container ${CONTAINER} is not running" >&2
    exit 1
fi

# Build the publish args. NO_ALIAS=1 skips the latest pointer.
publish_args=(--db "${DB_PATH_IN_CONTAINER}")
if [[ -z "${NO_ALIAS:-}" ]]; then
    publish_args+=(--alias "${ALIAS_KEY}")
fi

# Run the publish. Capture all stdout; the bare key is the last line.
# Send stderr through so operators see diagnostics in their cron mail.
out=$(docker exec "${CONTAINER}" /usr/local/bin/txco snapshot publish "${publish_args[@]}")
echo "${out}"

# Surface only the bare key on the FINAL line. Cron pipelines can use
# `tail -n1` to capture it for downstream automation.
key=$(echo "${out}" | tail -n1)
if [[ -z "${key}" || "${key}" == "published "* ]]; then
    echo "FAIL: couldn't capture snapshot key from publish output" >&2
    exit 1
fi

# Final-line-is-key contract for any caller piping us:
echo "${key}"
