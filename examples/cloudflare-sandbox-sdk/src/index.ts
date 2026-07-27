import { getSandbox } from '@cloudflare/sandbox';

export { Sandbox } from '@cloudflare/sandbox';

type Env = WorkerBindings & {
  readonly SANDBOX_API_TOKEN?: string;
};

type MountRequest = {
  sandboxId?: string;
  remote?: string;
  branch?: string;
};

type MountConfig = {
  sandboxId: string;
  remote: string;
  branch: string;
  repoName: string;
  mountPath: string;
  env: Record<string, string>;
};

type StoredMountMetadata = {
  remote: string;
  branch: string;
  repoName: string;
  mountPath: string;
};

const DEFAULT_REMOTE = 'https://github.com/cloudflare/sandbox-sdk.git';
const DEFAULT_BRANCH = 'main';
const DEFAULT_MOUNT_ROOT = '/workspace/mnt';
const DEFAULT_ARTIFACT_FS_ROOT = '/tmp/artifact-fs';
const DEFAULT_METADATA_FILE = '/workspace/.artifact-fs-mount';
const MOUNT_SCRIPT = '/usr/local/bin/mount-artifact-fs-repo';
const MAX_REQUEST_BODY_BYTES = 16 * 1024;
const MOUNT_CONFLICT_EXIT_CODE = 42;

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (request.method === 'GET' && url.pathname === '/') {
      return new Response(helpText(), {
        headers: { 'Content-Type': 'text/plain; charset=utf-8' }
      });
    }

    if (request.method === 'POST' && url.pathname === '/mount') {
      return handleMount(request, env);
    }

    if (request.method === 'GET' && url.pathname === '/status') {
      return handleStatus(request, env);
    }

    return new Response('Not found', { status: 404 });
  }
} satisfies ExportedHandler<Env>;

async function handleMount(request: Request, env: Env): Promise<Response> {
  try {
    await authorizeRequest(request, env);
    const body = await parseMountRequest(request);
    const config = buildMountConfig(body.sandboxId, body.remote, body.branch);
    const sandbox = getSandbox(env.Sandbox, config.sandboxId, {
      enableDefaultSession: false,
      normalizeId: true,
      sleepAfter: '15m',
      transport: 'rpc'
    });

    const existingMetadata = await readMountMetadata(sandbox);
    const mountConflict = compareMountMetadata(existingMetadata, config);
    if (mountConflict) {
      return Response.json({ error: mountConflict }, { status: 409 });
    }

    // Run the bootstrap script with per-request env so one Worker can mount
    // different remotes in different sandboxes without rebuilding the image.
    let bootstrap;
    try {
      bootstrap = await sandbox.exec(MOUNT_SCRIPT, {
        cwd: '/workspace',
        env: config.env,
        timeout: 120_000
      });
    } catch (error) {
      // An exec timeout does not guarantee the bootstrap process stopped.
      try {
        await sandbox.destroy();
      } catch (destroyError) {
        logError('sandbox cleanup failed', destroyError, request);
      }
      throw error;
    }

    if (!bootstrap.success) {
      if (bootstrap.exitCode === MOUNT_CONFLICT_EXIT_CODE) {
        throw new UserError(
          'Sandbox is already initialized for a different repo or branch',
          409
        );
      }

      const commandError = new SandboxCommandError(
        'ArtifactFS bootstrap failed',
        MOUNT_SCRIPT,
        bootstrap.exitCode
      );
      try {
        await sandbox.destroy();
      } catch (destroyError) {
        logError('sandbox cleanup failed', destroyError, request);
      }
      throw commandError;
    }

    const repo = await collectMountedRepoState(
      sandbox,
      config.repoName,
      config.mountPath
    );

    return Response.json({
      sandboxId: config.sandboxId,
      remote: config.remote,
      branch: config.branch,
      repoName: config.repoName,
      mountPath: config.mountPath,
      bootstrapLog: bootstrap.stdout.trim(),
      ...repo
    });
  } catch (error) {
    return errorResponse(error, request);
  }
}

async function handleStatus(request: Request, env: Env): Promise<Response> {
  try {
    await authorizeRequest(request, env);
    const url = new URL(request.url);
    const sandboxId = url.searchParams.get('sandboxId');
    if (!sandboxId) {
      return Response.json(
        { error: 'Missing sandboxId query parameter' },
        { status: 400 }
      );
    }

    const config = buildStatusConfig(
      sandboxId,
      url.searchParams.get('remote'),
      url.searchParams.get('branch')
    );
    const sandbox = getSandbox(env.Sandbox, config.sandboxId, {
      enableDefaultSession: false,
      normalizeId: true,
      sleepAfter: '15m',
      transport: 'rpc'
    });

    const metadata = await readMountMetadata(sandbox);
    if (!metadata) {
      return Response.json(
        {
          error:
            'No active mount found. The sandbox may have slept; POST /mount again to recreate it.'
        },
        { status: 404 }
      );
    }

    const mountConflict = compareMountMetadata(metadata, {
      remote: config.remote,
      branch: config.branch,
      repoName: metadata.repoName,
      mountPath: metadata.mountPath
    });
    if (mountConflict) {
      return Response.json({ error: mountConflict }, { status: 409 });
    }

    const repo = await collectMountedRepoState(
      sandbox,
      metadata.repoName,
      metadata.mountPath
    );

    return Response.json({
      sandboxId: normalizeSandboxId(sandboxId),
      remote: metadata.remote,
      branch: metadata.branch,
      repoName: metadata.repoName,
      mountPath: metadata.mountPath,
      ...repo
    });
  } catch (error) {
    return errorResponse(error, request);
  }
}

async function parseMountRequest(request: Request): Promise<MountRequest> {
  let body: unknown;

  try {
    body = JSON.parse(await readRequestBody(request));
  } catch (error) {
    if (error instanceof UserError) {
      throw error;
    }
    throw new UserError('Request body must be valid JSON', 400);
  }

  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    throw new UserError('Request body must be a JSON object', 400);
  }

  const { sandboxId, remote, branch } = body as Record<string, unknown>;
  validateOptionalString('sandboxId', sandboxId);
  validateOptionalString('remote', remote);
  validateOptionalString('branch', branch);

  return {
    sandboxId,
    remote,
    branch
  };
}

async function readRequestBody(request: Request): Promise<string> {
  if (!request.body) {
    return '';
  }

  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let totalBytes = 0;

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }

      totalBytes += value.byteLength;
      if (totalBytes > MAX_REQUEST_BODY_BYTES) {
        await reader.cancel();
        throw new UserError('Request body is too large', 413);
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }

  const body = new Uint8Array(totalBytes);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(body);
}

function buildMountConfig(
  sandboxIdInput?: string | null,
  remoteInput?: string | null,
  branchInput?: string | null
): MountConfig {
  const remote = normalizeRemote(remoteInput ?? DEFAULT_REMOTE);
  const branch = (branchInput ?? DEFAULT_BRANCH).trim() || DEFAULT_BRANCH;
  validateBranch(branch);
  const repoName = inferRepoName(remote);
  const sandboxId = sandboxIdInput
    ? normalizeRequestedSandboxId(sandboxIdInput)
    : normalizeSandboxId(crypto.randomUUID());
  const mountPath = `${DEFAULT_MOUNT_ROOT}/${repoName}`;

  return {
    sandboxId,
    remote,
    branch,
    repoName,
    mountPath,
    env: {
      MOUNT_GIT_REMOTE: remote,
      MOUNT_GIT_BRANCH: branch,
      ARTIFACT_FS_ROOT: DEFAULT_ARTIFACT_FS_ROOT,
      MOUNT_ROOT: DEFAULT_MOUNT_ROOT,
      ARTIFACT_FS_MOUNT_METADATA_FILE: DEFAULT_METADATA_FILE
    }
  };
}

function buildStatusConfig(
  sandboxIdInput: string,
  remoteInput?: string | null,
  branchInput?: string | null
) {
  const branch = branchInput?.trim() || undefined;
  if (branch) {
    validateBranch(branch);
  }

  return {
    sandboxId: normalizeRequestedSandboxId(sandboxIdInput),
    remote: remoteInput ? normalizeRemote(remoteInput) : undefined,
    branch
  };
}

function normalizeRemote(value: string): string {
  const remote = value.trim();
  if (remote.length > 2048 || /[\u0000-\u0020\u007f]/.test(remote)) {
    throw new UserError('remote contains invalid characters or is too long', 400);
  }

  if (remote.startsWith('https://') || remote.startsWith('ssh://')) {
    let parsed: URL;
    try {
      parsed = new URL(remote);
    } catch {
      throw new UserError('remote must be a valid Git URL', 400);
    }

    if (
      (parsed.protocol === 'https:' && (parsed.username || parsed.password)) ||
      (parsed.protocol === 'ssh:' && parsed.password)
    ) {
      throw new UserError(
        'remote must not include credentials; use credential helpers or SSH auth instead',
        400
      );
    }

    if (parsed.search || parsed.hash) {
      throw new UserError(
        'remote must not include query parameters or fragments',
        400
      );
    }

    if (!parsed.pathname || parsed.pathname === '/') {
      throw new UserError('remote must include a repository path', 400);
    }

    return remote;
  }

  if (/^[^@:\s]+@[^:\s]+:[^\s?#]+$/.test(remote)) {
    return remote;
  }

  throw new UserError('remote must be an HTTPS or SSH Git URL', 400);
}

function inferRepoName(remote: string): string {
  const trimmed = remote.replace(/\/+$/, '').replace(/\.git$/, '');
  const lastSeparator = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf(':'));
  const repoName = trimmed.slice(lastSeparator + 1);

  if (
    !repoName ||
    repoName.length > 100 ||
    !/^[a-zA-Z0-9._-]+$/.test(repoName) ||
    repoName === '.' ||
    repoName === '..'
  ) {
    throw new UserError(
      'repository name must use 1-100 letters, numbers, dots, hyphens, or underscores',
      400
    );
  }

  return repoName;
}

function normalizeSandboxId(value: string): string {
  return value.trim().toLowerCase();
}

function normalizeRequestedSandboxId(value: string): string {
  const normalized = normalizeSandboxId(value);

  if (!normalized) {
    throw new UserError('sandboxId must be 1-63 characters long', 400);
  }

  if (normalized.length > 63) {
    throw new UserError('sandboxId must be 1-63 characters long', 400);
  }

  if (normalized.startsWith('-') || normalized.endsWith('-')) {
    throw new UserError('sandboxId cannot start or end with hyphens', 400);
  }

  if (!/^[a-z0-9.-]+$/.test(normalized)) {
    throw new UserError(
      'sandboxId may contain only letters, numbers, dots, and hyphens',
      400
    );
  }

  if (!/[a-z0-9]/.test(normalized)) {
    throw new UserError('sandboxId must include at least one letter or number', 400);
  }

  if (
    ['www', 'api', 'admin', 'root', 'system', 'cloudflare', 'workers'].includes(
      normalized
    )
  ) {
    throw new UserError('sandboxId is reserved', 400);
  }

  return normalized;
}

async function collectMountedRepoState(
  sandbox: ReturnType<typeof getSandbox>,
  repoName: string,
  mountPath: string
) {
  const artifactFsStatus = await runChecked(
    sandbox,
    `artifact-fs status --name ${shellQuote(repoName)}`,
    'Could not read ArtifactFS status'
  );

  const mounted = /(^|\s)state=mounted(\s|$)/.test(artifactFsStatus.stdout);
  if (!mounted) {
    return {
      head: null,
      gitStatus: null,
      artifactFsStatus: artifactFsStatus.stdout.trim()
    };
  }

  const [head, gitStatus] = await Promise.all([
    runChecked(
      sandbox,
      `git -C ${shellQuote(mountPath)} rev-parse HEAD`,
      'Could not read mounted HEAD'
    ),
    runChecked(
      sandbox,
      `git -C ${shellQuote(mountPath)} status --short --branch`,
      'Could not read mounted git status'
    )
  ]);

  return {
    head: head.stdout.trim(),
    gitStatus: gitStatus.stdout.trim(),
    artifactFsStatus: artifactFsStatus.stdout.trim()
  };
}

async function readMountMetadata(
  sandbox: ReturnType<typeof getSandbox>
): Promise<StoredMountMetadata | null> {
  const metadata = await sandbox.exists(DEFAULT_METADATA_FILE);
  if (!metadata.exists) {
    return null;
  }

  const file = await sandbox.readFile(DEFAULT_METADATA_FILE);
  return parseMetadataFile(file.content);
}

function compareMountMetadata(
  existing: StoredMountMetadata | null,
  requested: {
    remote?: string;
    branch?: string;
    repoName: string;
    mountPath: string;
  }
): string | null {
  if (!existing) {
    return null;
  }

  if (requested.remote && existing.remote !== requested.remote) {
    return `Sandbox is mounted for ${existing.remote}, not ${requested.remote}`;
  }

  if (requested.branch && existing.branch !== requested.branch) {
    return `Sandbox is mounted for branch ${existing.branch}, not ${requested.branch}`;
  }

  if (
    existing.repoName !== requested.repoName ||
    existing.mountPath !== requested.mountPath
  ) {
    return 'Sandbox metadata does not match the requested mount layout';
  }

  return null;
}

function parseMetadataFile(content: string): StoredMountMetadata {
  const values = new Map<string, string>();

  for (const line of content.split('\n')) {
    if (!line) {
      continue;
    }
    const index = line.indexOf('=');
    if (index === -1) {
      continue;
    }
    const key = line.slice(0, index);
    const rawValue = line.slice(index + 1).trim();
    values.set(key, rawValue);
  }

  const remote = values.get('MOUNTED_REMOTE');
  const branch = values.get('MOUNTED_BRANCH');
  const repoName = values.get('MOUNTED_REPO_NAME');
  const mountPath = values.get('MOUNTED_MOUNT_PATH');

  if (!remote || !branch || !repoName || !mountPath) {
    throw new Error('Mount metadata is missing required fields');
  }

  return { remote, branch, repoName, mountPath };
}

function validateOptionalString(
  name: string,
  value: unknown
): asserts value is string | undefined {
  if (value !== undefined && typeof value !== 'string') {
    throw new UserError(`${name} must be a string`, 400);
  }
}

function validateBranch(branch: string): void {
  const invalidBase =
    !branch ||
    branch === '@' ||
    branch.startsWith('-') ||
    branch.startsWith('/') ||
    branch.endsWith('/') ||
    branch.endsWith('.') ||
    branch.includes('..') ||
    branch.includes('//') ||
    branch.includes('@{') ||
    /[\x00-\x20~^:?*[\\]/.test(branch);

  if (invalidBase) {
    throw new UserError('branch must be a valid Git branch name', 400);
  }

  for (const component of branch.split('/')) {
    if (!component || component.startsWith('.') || component.endsWith('.lock')) {
      throw new UserError('branch must be a valid Git branch name', 400);
    }
  }
}

function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'"'"'`)}'`;
}

function helpText(): string {
  return [
    'ArtifactFS + Cloudflare Sandbox SDK example',
    '',
    'POST /mount with JSON: {"sandboxId":"demo","remote":"https://github.com/cloudflare/sandbox-sdk.git","branch":"main"}',
    'GET  /status?sandboxId=demo&remote=https://github.com/cloudflare/sandbox-sdk.git',
    '',
    'The Docker image includes ArtifactFS and a bootstrap script that mounts the requested repo inside /workspace/mnt/<repo-name>.'
  ].join('\n');
}

async function runChecked(
  sandbox: ReturnType<typeof getSandbox>,
  command: string,
  message: string,
  options?: { cwd?: string; env?: Record<string, string>; timeout?: number }
) {
  const result = await sandbox.exec(command, options);
  if (!result.success) {
    throw new SandboxCommandError(message, command, result.exitCode);
  }
  return result;
}

function errorResponse(error: unknown, request: Request): Response {
  if (error instanceof UserError) {
    const headers =
      error.status === 401 ? { 'WWW-Authenticate': 'Bearer' } : undefined;
    return Response.json(
      { error: error.message },
      { status: error.status, headers }
    );
  }

  logError('request failed', error, request);
  return Response.json({ error: 'Internal server error' }, { status: 500 });
}

function logError(message: string, error: unknown, request: Request): void {
  const details =
    error instanceof SandboxCommandError
      ? {
          error: error.message,
          command: error.command,
          exitCode: error.exitCode
        }
      : {
          error: error instanceof Error ? error.message : String(error)
        };

  console.error(
    JSON.stringify({
      message,
      method: request.method,
      path: new URL(request.url).pathname,
      ...details
    })
  );
}

async function authorizeRequest(request: Request, env: Env): Promise<void> {
  const configuredToken = env.SANDBOX_API_TOKEN ?? '';
  if (!configuredToken) {
    throw new Error('SANDBOX_API_TOKEN is not configured');
  }

  const header = (request.headers.get('authorization') ?? '').trim();
  const match = /^Bearer ([^\s]+)$/i.exec(header);
  const tokenMatches = await timingSafeEqual(match?.[1] ?? '', configuredToken);
  if (!match || !tokenMatches) {
    throw new UserError('Unauthorized', 401);
  }
}

async function timingSafeEqual(left: string, right: string): Promise<boolean> {
  const encoder = new TextEncoder();
  const [leftHash, rightHash] = await Promise.all([
    crypto.subtle.digest('SHA-256', encoder.encode(left)),
    crypto.subtle.digest('SHA-256', encoder.encode(right))
  ]);
  return crypto.subtle.timingSafeEqual(leftHash, rightHash);
}

class SandboxCommandError extends Error {
  constructor(
    message: string,
    readonly command: string,
    readonly exitCode: number
  ) {
    super(message);
  }
}

class UserError extends Error {
  constructor(
    message: string,
    readonly status: number
  ) {
    super(message);
  }
}
