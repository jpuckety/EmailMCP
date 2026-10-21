# email-mcp

A production-ready **MCP (Model Context Protocol)** server written in **Go** that
exposes IMAP and SMTP email capabilities to MCP clients. It is simultaneously:

- an **OAuth 2.1 Authorization Server** fronting **Google OIDC**,
- a **Resource Server** guarding MCP tool calls with opaque bearer tokens, and
- an MCP server exposing email tools.

Storage is **DynamoDB** (durable, default) or **in-memory** (fallback). IMAP/SMTP
credentials live as attributes on the user-config table, encrypted at rest with a
customer-managed KMS key.

AWS deploy, image promotion, and SSM secrets are owned by
[MCPCICD](https://github.com/jpuckety/MCPCICD). This repo is the application plus
the env CloudFormation stack (`EmailMcpStack`) that MCPCICD deploys into Dev/Prod.

## Layout

| Path | Role |
|---|---|
| `emailmcp/` | Go MCP server & embedded static UI |
| `web/` | Angular frontend for email account management |
| `cdk/` | Env stack: VPC, ALB, ECS CodeDeploy blue/green, DynamoDB, WAF |
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

PRs run GitHub Actions only (`go test ./...` + `docker build`). Merges to
`master` are released by the MCPCICD pipeline named `email-mcp`.

New in 0.2.0: `create_draft`, `get_attachment`, HTTPS/path attachments, replies/forwards,
and Sent-folder copies. See [emailmcp/README.md](emailmcp/README.md) and [CHANGELOG.md](CHANGELOG.md).

## Pipeline contract

MCPCICD expects:

- Repo-root `Dockerfile`
- `cdk/lib/email-mcp-stack.ts` with `DeploymentControllerType.CODE_DEPLOY` and
  `TASK_FAMILY = 'email-mcp'`
- `cdk/scripts/prepare-taskdef.js`
- Container `email-mcp` on port `8080` with `GET /health`
- SSM SecureString parameters in each env account:
  - `/email-mcp/google-client-id`
  - `/email-mcp/google-client-secret`

Hostnames (MCPCICD context): `email-dev.mcp.ecg.co` (Dev) and `email.mcp.ecg.co` (Prod).
