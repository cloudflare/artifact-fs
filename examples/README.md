# Container example

Build ArtifactFS and run a FUSE-mounted public repository:

```bash
docker build -t artifact-fs-example -f examples/Dockerfile .
docker run --rm --cap-add SYS_ADMIN --device /dev/fuse \
  artifact-fs-example git log --oneline -5
```

Set `REPO_REMOTE_URL`, `REPO_BRANCH`, and `REPO_NAME` to mount another repository.
For a private HTTPS remote, keep credentials out of the URL and mount a Git credential file:

```text
https://x-access-token:<token>@github.com
```

```bash
docker run --rm --cap-add SYS_ADMIN --device /dev/fuse \
  --mount type=bind,src=/path/to/git-credentials,dst=/run/secrets/git-credentials,readonly \
  -e GIT_CONFIG_COUNT=1 \
  -e GIT_CONFIG_KEY_0=credential.helper \
  -e 'GIT_CONFIG_VALUE_0=store --file=/run/secrets/git-credentials' \
  -e REPO_REMOTE_URL=https://github.com/org/private-repo.git \
  artifact-fs-example
```

On AppArmor hosts, also pass `--security-opt apparmor:unconfined`.
