# email-mcp

A production-ready **MCP (Model Context Protocol)** server written in **Go** that
exposes IMAP and SMTP email capabilities to MCP clients. It is simultaneously:

- an **OAuth 2.1 Authorization Server** fronting **Google OIDC**,
- a **Resource Server** guarding MCP tool calls with opaque bearer tokens, and
- an MCP server exposing email tools.

Storage is **DynamoDB** (durable, default) or **in-memory** (fallback). IMAP/SMTP
credentials live as attributes on the user-config table, encrypted at rest with a
customer-managed KMS key.

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
| `cdk/` | Env stack: VPC, ALB, ECS CodeDeploy blue/green, notify Fargate service, DynamoDB, WAF |
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
- `cdk/lib/email-mcp-stack.ts` with `DeploymentControllerType.CODE_DEPLOY` and
  `TASK_FAMILY = 'email-mcp'`
- `cdk/scripts/prepare-taskdef.js`
- Container `email-mcp` on port `8080` with `GET /health`
- SSM SecureString parameters in each env account (push from this repo before
  the first CdkDev):
  - `/email-mcp/google-client-id`
  - `/email-mcp/google-client-secret`

Hostnames (MCPCICD context): `email-dev.mcp.ecg.co` (Dev) and `email.mcp.ecg.co` (Prod).

MCPCICD CodeDeploy still updates **only** the ALB-backed `email-mcp` service.
`prepare-taskdef.js` is unchanged. The IMAP IDLE watcher is a second ECS
service (`email-mcp-notify`, family `email-mcp-notify`) with no ALB and no
CodeDeploy. MCPCICD is not modified for notify rolls.

MCPCICD `CdkDev`/`CdkProd` omit `imageUri`. **Dev has no Promote stage**
(`CdkDev` runs before Build); the **Prod** order is `CdkProd` → `Promote` →
`DeployProd`: `CdkProd` runs before Promote, and `DeployProd` CodeDeploys only
`email-mcp`. Those env-stack deploys do **not** put a real watcher binary on
`email-mcp-notify` by themselves. The notify image is a provisioning-time
GetAtt of SSM `/email-mcp/notify/image-uri` (created as the busybox `/health`
placeholder on first deploy and **not** overwritten when `imageUri` is
omitted). Until that parameter holds a promoted `email-mcp` digest, the
service is inert: it answers `GET /health` but does not IDLE or POST webhooks.

After **Dev Build** or **Prod Promote** (manual follow-up), write the digest
from that environment's ECR account. For Prod, do this **after** Promote and
use the promoted Prod-account digest; the preceding `CdkProd` cannot pick it
up, and `DeployProd` does not roll the notify service:

1. `aws ssm put-parameter --name /email-mcp/notify/image-uri --type String --overwrite --value <ecr-uri@sha256:digest>`
2. To run it **immediately**, copy the current `email-mcp-notify` task
   definition revision and change **only** the `image` of **both** containers
   (`email-mcp-notify` and `volume-init`). Keep the bootstrap
   `entryPoint: ["sh","-c"]` and command — it already `exec`s
   `/app/emailmcp -transport notify` when that binary exists. Then
   `aws ecs update-service --cluster email-mcp --service email-mcp-notify --task-definition email-mcp-notify:<rev>`.
   Replacing the command with `-transport notify` while `entryPoint` is still
   `sh -c` runs `sh -c -transport notify` and the task crash-loops.
3. Or do a break-glass `EmailMcpStack` redeploy with `-c imageUri=<digest>`.
   This bakes the digest into the task definition and overwrites SSM. Pass the
   same context values MCPCICD uses; for example:

   ```bash
   cd cdk && npx cdk deploy EmailMcpStack \
     -c hostname=<MCPCICD-hostname> \
     -c pipelineAccount=<pipeline-account-id> \
     -c containerEnv='<same JSON passed by MCPCICD>' \
     -c hostedZoneName=<hosted-zone-name> \
     -c hostedZoneId=<hosted-zone-id> \
     -c imageUri=<environment-account-ecr-uri@sha256:digest>
   ```

   Use the same `hostname`, `pipelineAccount`, `containerEnv`, and
   `hostedZoneName`/`hostedZoneId` values MCPCICD passes for that environment.
   Omitting `pipelineAccount` can remove pipeline SSM parameters and role
   outputs; omitting `containerEnv` can drop environment-specific task settings.

`--force-new-deployment` alone does **not** change a URI already baked into
the running revision. The next `EmailMcpStack` deploy re-reads SSM and
updates the notify task definition image if the parameter changed. A **bad
digest** in SSM can fail the notify circuit breaker and CloudFormation
**rolls back the whole stack** (including HTTP `email-mcp`); later pipeline
runs keep failing until SSM is fixed. Do not register a one-off task
definition without writing SSM: the next env-stack deploy replaces the
revision with the SSM image.

Leave `EMAILMCP_NOTIFY_OWNER` unset so each task generates a unique lease
owner. The notify task does not use Google OAuth secrets; it requires
`EMAILMCP_USER_CONFIG_TABLE` and `EMAILMCP_NOTIFY_STATE_TABLE`.
