# email-mcp

A production-ready **MCP (Model Context Protocol)** server written in **Go** that
exposes IMAP and SMTP email capabilities to MCP clients. It is simultaneously:

- an **OAuth 2.1 Authorization Server** fronting **Google OIDC**,
- a **Resource Server** guarding MCP tool calls with opaque bearer tokens, and
- an MCP server exposing email tools.

Storage is **DynamoDB** (durable, default) or **in-memory** (fallback). IMAP/SMTP
credentials live as attributes on the user-config table, encrypted with a
customer-managed KMS key before they are written; DynamoDB also encrypts the
table at rest. Existing plaintext credentials remain readable and are encrypted
on the next account update. ECS configures this with `EMAILMCP_KMS_KEY_ID`.

AWS deploy and image promotion are owned by
[MCPCICD](https://github.com/jpuckety/MCPCICD). This repo is the application plus
the env CloudFormation stack (`EmailMcpStack`) that MCPCICD deploys into Dev/Prod.
SSM Google OAuth secrets for this app are `./run.sh push-secrets-dev` /
`push-secrets-prod`.

## Layout

| Path | Role |
|---|---|
| `emailmcp/` | Go MCP server & embedded static UI |
| `web/` | Angular frontend for email account management |
| `cdk/` | Env stack: VPC, ALB, ECS CodeDeploy blue/green for `email-mcp` and `email-mcp-notify`, DynamoDB, WAF |
| `Dockerfile` | Multi-stage image build (Angular + Go static binary) |

## Local development

```bash
./run.sh help
./run.sh test
./run.sh run
./run.sh docker-build
./run.sh synth
```

Copy [emailmcp/.env.example](emailmcp/.env.example) to `emailmcp/.env` for local
HTTP mode (`GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `PUBLIC_BASE_URL`).
For AWS, copy [`.env.example`](.env.example) to `.env.dev` / `.env.prod` and run
`./run.sh push-secrets-dev` / `push-secrets-prod`. Those commands use AWS
profiles `mcp-dev` / `mcp-prod` (MCPCICD `./run.sh child-profile`) unless you
set `DEV_PROFILE` / `PROD_PROFILE`. Do not use default credentials — they
target the wrong account.

PRs run GitHub Actions only (`go test ./...` + `docker build`). Merges to
`master` are released by the MCPCICD pipeline named `email-mcp`.

New in 0.2.0: `create_draft`, `get_attachment`, HTTPS/path attachments, replies/forwards,
and Sent-folder copies. See [emailmcp/README.md](emailmcp/README.md) and [CHANGELOG.md](CHANGELOG.md).

## Pipeline contract

MCPCICD expects:

- Repo-root `Dockerfile`
- `cdk/lib/email-mcp-stack.ts` with `DeploymentControllerType.CODE_DEPLOY` for
  both `TASK_FAMILY = 'email-mcp'` and `NOTIFY_TASK_FAMILY = 'email-mcp-notify'`
- `cdk/scripts/prepare-taskdef.js` (optional second arg is the task family;
  `email-mcp-notify` sets `command` to `-transport notify`)
- Containers `email-mcp` and `email-mcp-notify` on port `8080` with `GET /health`
- CodeDeploy applications/groups `email-mcp` and `email-mcp-notify`
- SSM SecureString parameters in each env account (push from this repo before
  the first CdkDev):
  - `/email-mcp/google-client-id`
  - `/email-mcp/google-client-secret`

Hostnames (MCPCICD context): `email-dev.mcp.ecg.co` (Dev) and `email.mcp.ecg.co` (Prod).

MCPCICD `CdkDev`/`CdkProd` omit `imageUri`, so both task families register the
busybox `/health` placeholder. **Dev** has no Promote stage (`CdkDev` runs
before Build); **Prod** order is `CdkProd` → `Promote` → `DeployProd`.
`DeployDev` / `DeployProd` CodeDeploy **both** services: HTTP `email-mcp` and
IMAP IDLE `email-mcp-notify`. Notify uses a private ALB listener on port 8081
(not opened to the internet) so ECS CodeDeploy can health-check `/health`
during blue/green. Redeploy the MCPCICD EmailMCP pipeline after this change so
those stages include the `email-mcp-notify` CodeDeploy action.

`prepare-taskdef.js` describes each family, substitutes `<IMAGE1_NAME>` on the
app container, strips the placeholder `entryPoint`/`command` for HTTP, and for
notify sets `command: ["-transport","notify"]` so the image ENTRYPOINT runs the
watcher. The `volume-init` sidecar is left unchanged.

Leave `EMAILMCP_NOTIFY_OWNER` unset so each task generates a unique lease
owner. The notify task does not use Google OAuth secrets; it requires
`EMAILMCP_USER_CONFIG_TABLE` and `EMAILMCP_NOTIFY_STATE_TABLE`.
