# Cloudflare Sandbox SDK example

This Worker starts a Cloudflare Sandbox and mounts a public Git repository at `/workspace/mnt/<repo>`.
It requires Docker, Node.js 24, and a Workers Paid plan.

```bash
cd examples/cloudflare-sandbox-sdk
npm ci
cp .dev.vars.example .dev.vars
npm run dev
```

Mount the default repository:

```bash
curl -X POST http://localhost:8787/mount \
  -H 'authorization: Bearer local-dev-token' \
  -H 'content-type: application/json' \
  -d '{"sandboxId":"demo"}'
```

Check the active mount:

```bash
curl -H 'authorization: Bearer local-dev-token' \
  'http://localhost:8787/status?sandboxId=demo'
```

The container sleeps after 15 minutes of inactivity. Repeat `POST /mount` to recreate an expired mount.

Deploy with a production bearer token:

```bash
npx wrangler secret put SANDBOX_API_TOKEN
npm run deploy
```
