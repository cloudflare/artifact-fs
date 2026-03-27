#!/usr/bin/env bash
set -euo pipefail

: "${REPO_REMOTE_URL:=https://github.com/cloudflare/workers-sdk.git}"
: "${REPO_BRANCH:=main}"
: "${REPO_NAME:=repo}"
: "${ARTIFACT_FS_ROOT:=/var/lib/artifact-fs}"
: "${MOUNT_ROOT:=/mnt}"

export ARTIFACT_FS_ROOT

# Redact credentials from the display URL to avoid leaking tokens in logs.
DISPLAY_URL=$(echo "${REPO_REMOTE_URL}" | sed -E 's|://[^@]+@|://REDACTED@|')
echo "artifact-fs: registering ${REPO_NAME} from ${DISPLAY_URL} (branch: ${REPO_BRANCH})"

artifact-fs add-repo \
  --name "${REPO_NAME}" \
  --remote "${REPO_REMOTE_URL}" \
  --branch "${REPO_BRANCH}" \
  --mount-root "${MOUNT_ROOT}"

echo "artifact-fs: starting daemon (mount at ${MOUNT_ROOT}/${REPO_NAME})"
artifact-fs daemon --root "${MOUNT_ROOT}" &
DAEMON_PID=$!

# Wait for the FUSE mount, checking that the daemon is still alive.
MOUNT_PATH="${MOUNT_ROOT}/${REPO_NAME}"
for i in $(seq 1 120); do
  if ! kill -0 "${DAEMON_PID}" 2>/dev/null; then
    echo "artifact-fs: daemon exited unexpectedly" >&2
    wait "${DAEMON_PID}" || true
    exit 1
  fi
  if mountpoint -q "${MOUNT_PATH}" 2>/dev/null; then
    break
  fi
  sleep 0.5
done

if ! mountpoint -q "${MOUNT_PATH}" 2>/dev/null; then
  echo "artifact-fs: mount did not appear at ${MOUNT_PATH}" >&2
  exit 1
fi

echo "artifact-fs: mounted at ${MOUNT_PATH}"
ls "${MOUNT_PATH}"

# Run the user command, then clean up the daemon.
if [ $# -gt 0 ]; then
  cd "${MOUNT_PATH}"
  "$@"
  EXIT_CODE=$?
  kill "${DAEMON_PID}" 2>/dev/null || true
  wait "${DAEMON_PID}" 2>/dev/null || true
  exit ${EXIT_CODE}
else
  # No command -- keep the container alive with the daemon.
  wait "${DAEMON_PID}"
fi
