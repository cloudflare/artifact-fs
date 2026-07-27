#!/usr/bin/env bash
set -euo pipefail

: "${MOUNT_GIT_REMOTE:=https://github.com/cloudflare/sandbox-sdk.git}"
: "${MOUNT_GIT_BRANCH:=main}"
: "${ARTIFACT_FS_ROOT:=/tmp/artifact-fs}"
: "${MOUNT_ROOT:=/workspace/mnt}"
: "${ARTIFACT_FS_MOUNT_METADATA_FILE:=/workspace/.artifact-fs-mount}"
: "${ARTIFACT_FS_DAEMON_LOG:=/tmp/artifact-fs-daemon.log}"
: "${ARTIFACT_FS_DAEMON_PID_FILE:=/tmp/artifact-fs-daemon.pid}"
: "${ARTIFACT_FS_MOUNT_LOCK_FILE:=/tmp/artifact-fs-mount.lock}"

readonly MOUNT_CONFLICT_EXIT_CODE=42

exec 9>"$ARTIFACT_FS_MOUNT_LOCK_FILE"
if ! flock -w 120 9; then
  echo "artifact-fs: timed out waiting for another mount operation" >&2
  exit 1
fi

validate_remote() {
  local remote="$1"

  if [ "${#remote}" -gt 2048 ] || [[ "$remote" =~ [[:cntrl:][:space:]] ]]; then
    echo "artifact-fs: MOUNT_GIT_REMOTE contains invalid characters or is too long" >&2
    exit 1
  fi

  if [[ "$remote" != https://* && "$remote" != ssh://* && ! "$remote" =~ ^[^@:[:space:]]+@[^:[:space:]]+:[^[:space:]?#]+$ ]]; then
    echo "artifact-fs: MOUNT_GIT_REMOTE must be an HTTPS or SSH remote" >&2
    exit 1
  fi

  case "$remote" in
    https://*\?*|https://*#*|ssh://*\?*|ssh://*#*)
      echo "artifact-fs: MOUNT_GIT_REMOTE must not include query parameters or fragments" >&2
      exit 1
      ;;
  esac

  if [[ "$remote" == https://* && "$remote" =~ ^https://[^/]*@ ]]; then
    echo "artifact-fs: HTTPS remotes must not include credentials" >&2
    exit 1
  fi

  if [[ "$remote" == ssh://* ]]; then
    local authority="${remote#ssh://}"
    authority="${authority%%/*}"
    if [[ "$authority" == *:*@* ]]; then
      echo "artifact-fs: SSH remotes must not include passwords" >&2
      exit 1
    fi
  fi
}

infer_repo_name() {
  local remote="$1"
  remote="${remote%/}"
  remote="${remote%.git}"

  local name="${remote##*/}"
  if [ -z "$name" ] || [ "$name" = "$remote" ]; then
    name="${remote##*:}"
  fi

  if [ -z "$name" ] || [ "$name" = "$remote" ] || [ "${#name}" -gt 100 ] || [[ ! "$name" =~ ^[a-zA-Z0-9._-]+$ ]] || [ "$name" = "." ] || [ "$name" = ".." ]; then
    echo "artifact-fs: could not infer repo name from ${remote}" >&2
    exit 1
  fi

  printf '%s\n' "$name"
}

display_remote() {
  printf '%s' "$1" | sed -E 's#://[^@]+@#://REDACTED@#'
}

load_existing_metadata() {
  if [ -f "$ARTIFACT_FS_MOUNT_METADATA_FILE" ]; then
    while IFS='=' read -r key value; do
      case "$key" in
        MOUNTED_REMOTE) MOUNTED_REMOTE="$value" ;;
        MOUNTED_BRANCH) MOUNTED_BRANCH="$value" ;;
        MOUNTED_REPO_NAME) MOUNTED_REPO_NAME="$value" ;;
        MOUNTED_MOUNT_PATH) MOUNTED_MOUNT_PATH="$value" ;;
      esac
    done <"$ARTIFACT_FS_MOUNT_METADATA_FILE"
  fi
}

write_metadata() {
  local metadata_tmp="${ARTIFACT_FS_MOUNT_METADATA_FILE}.tmp.$$"
  cat >"$metadata_tmp" <<EOF
MOUNTED_REMOTE=$MOUNT_GIT_REMOTE
MOUNTED_BRANCH=$MOUNT_GIT_BRANCH
MOUNTED_REPO_NAME=$REPO_NAME
MOUNTED_MOUNT_PATH=$MOUNT_PATH
EOF
  mv "$metadata_tmp" "$ARTIFACT_FS_MOUNT_METADATA_FILE"
}

ensure_mount_matches_request() {
  if [ -z "${MOUNTED_REMOTE:-}" ]; then
    return 0
  fi

  if [ "$MOUNTED_REMOTE" != "$MOUNT_GIT_REMOTE" ] || [ "$MOUNTED_BRANCH" != "$MOUNT_GIT_BRANCH" ]; then
    echo "artifact-fs: sandbox already initialized for ${MOUNTED_REMOTE} on branch ${MOUNTED_BRANCH}" >&2
    echo "artifact-fs: use a different sandboxId for a different repo or branch" >&2
    exit "$MOUNT_CONFLICT_EXIT_CODE"
  fi

  if [ "${MOUNTED_REPO_NAME:-}" != "$REPO_NAME" ] || [ "${MOUNTED_MOUNT_PATH:-}" != "$MOUNT_PATH" ]; then
    echo "artifact-fs: stored mount metadata does not match the requested repo layout" >&2
    exit "$MOUNT_CONFLICT_EXIT_CODE"
  fi
}

ensure_daemon() {
  if [ -f "$ARTIFACT_FS_DAEMON_PID_FILE" ]; then
    local existing_pid
    existing_pid=$(cat "$ARTIFACT_FS_DAEMON_PID_FILE" 2>/dev/null || true)
    if [ -n "$existing_pid" ] && kill -0 "$existing_pid" 2>/dev/null; then
      return
    fi
    rm -f "$ARTIFACT_FS_DAEMON_PID_FILE"
  fi

  echo "artifact-fs: starting daemon under ${MOUNT_ROOT}"
  nohup artifact-fs daemon --root "$MOUNT_ROOT" >"$ARTIFACT_FS_DAEMON_LOG" 2>&1 </dev/null &
  echo "$!" >"$ARTIFACT_FS_DAEMON_PID_FILE"
}

wait_for_mount() {
  local mount_path="$1"

  for _ in $(seq 1 120); do
    if [ -f "$ARTIFACT_FS_DAEMON_PID_FILE" ]; then
      local daemon_pid
      daemon_pid=$(cat "$ARTIFACT_FS_DAEMON_PID_FILE" 2>/dev/null || true)
      if [ -n "$daemon_pid" ] && ! kill -0 "$daemon_pid" 2>/dev/null; then
        echo "artifact-fs: daemon exited before the mount became available" >&2
        if [ -f "$ARTIFACT_FS_DAEMON_LOG" ]; then
          cat "$ARTIFACT_FS_DAEMON_LOG" >&2
        fi
        exit 1
      fi
    fi

    if [ -e "$mount_path/.git" ] && git -C "$mount_path" rev-parse HEAD >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done

  echo "artifact-fs: mount did not appear at ${mount_path}" >&2
  exit 1
}

if [ ! -e /dev/fuse ]; then
  echo "artifact-fs: /dev/fuse is not available in this sandbox" >&2
  exit 1
fi

validate_remote "$MOUNT_GIT_REMOTE"
if ! git check-ref-format --branch "$MOUNT_GIT_BRANCH" >/dev/null 2>&1; then
  echo "artifact-fs: MOUNT_GIT_BRANCH must be a valid Git branch name" >&2
  exit 1
fi

REPO_NAME=$(infer_repo_name "$MOUNT_GIT_REMOTE")
MOUNT_PATH="${MOUNT_ROOT}/${REPO_NAME}"
DISPLAY_REMOTE=$(display_remote "$MOUNT_GIT_REMOTE")

mkdir -p "$ARTIFACT_FS_ROOT" "$MOUNT_ROOT"
load_existing_metadata
ensure_mount_matches_request

if artifact-fs status --name "$REPO_NAME" >/dev/null 2>&1; then
  if [ -z "${MOUNTED_REMOTE:-}" ]; then
    echo "artifact-fs: found an existing ArtifactFS registration for ${REPO_NAME} without matching metadata" >&2
    echo "artifact-fs: use a new sandboxId or clear the sandbox state before reusing this name" >&2
    exit 1
  fi
else
  echo "artifact-fs: registering ${REPO_NAME} from ${DISPLAY_REMOTE}"
  artifact-fs add-repo \
    --name "$REPO_NAME" \
    --remote "$MOUNT_GIT_REMOTE" \
    --branch "$MOUNT_GIT_BRANCH" \
    --mount-root "$MOUNT_ROOT"
fi

write_metadata
ensure_daemon
wait_for_mount "$MOUNT_PATH"

printf 'repo_name=%s\n' "$REPO_NAME"
printf 'mount_path=%s\n' "$MOUNT_PATH"
