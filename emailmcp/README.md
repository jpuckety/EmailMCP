# EmailMCP

A production-grade **Model Context Protocol (MCP)** server written in Go that exposes full email capabilities (IMAP + SMTP) as MCP tools for AI agents.

## Features

### IMAP (Inbound)
- Credentials loaded from Amazon DynamoDB
- Robust per-account IMAP connection pooling with health checks and limits
- List folders
- Search emails (text, from, since, etc.)
- Read full emails (envelope + body + attachment metadata)
- Move, flag, and delete (single and bulk)

### Multi-User & Cloud Architecture
- **Google OAuth2 Authentication**: MCP OAuth 2.1 discovery + authorization-code flow that fronts Google sign-in; Google ID tokens authorize MCP traffic.
- **Remote Storage**: Account configuration and encrypted credentials in Amazon DynamoDB.
- **AWS CDK Infrastructure**: Fully automated resource provisioning using TypeScript.
- **Direct DynamoDB Storage**: The Go server reads/writes configuration directly to DynamoDB via the ECS task role.

### SMTP (Outbound)
- Send plain text + HTML emails
- Attachments via base64, HTTPS URL, or allowlisted local path
- Inline CID images, replies, and forwards
- Drafts (`create_draft`) saved to IMAP Drafts — never sent
- Per-account SMTP configuration with TLS support

### Multi-User Security
- One email account config per authenticated Google user
- Google ID token required on MCP HTTP traffic
- No local database or master encryption key

## Requirements

- Go 1.25+ (due to MCP SDK)
- Node.js & npm (for CDK infrastructure)
- AWS CLI configured with appropriate credentials
- Google OAuth2 Client ID
- DynamoDB Table Name (auto-resolvable via SSM)

## Cloud deployment

AWS deploy is owned by [MCPCICD](https://github.com/jpuckety/MCPCICD). This repo
supplies the env stack (`cdk/`) and the repo-root `Dockerfile`. Local helpers:

```bash
./run.sh test
./run.sh synth
```

## Local Quick Start

```bash
# 1. Clone and build
git clone https://github.com/jpuckett/EmailMCP
cd EmailMCP/emailmcp
go mod tidy
go build -o emailmcp ./cmd/emailmcp

# 2. Configure
cp .env.example .env
# Edit .env and set EMAILMCP_USER_CONFIG_TABLE, GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET,
# and PUBLIC_BASE_URL (EMAILMCP_USER_CONFIG_TABLE can also be resolved from SSM)

# 3. Run
./run.sh run
```

The server listens on `:8080` by default and serves the Streamable HTTP transport at `/`. Startup fails if the configured DynamoDB table is unreachable.

### Notify transport (local watcher)

`emailmcp -transport notify` runs IMAP IDLE + webhook delivery only. It does **not**
start MCP, OAuth, or the portal, and it does **not** require `GOOGLE_CLIENT_ID`.
It serves `GET /health` on `EMAILMCP_LISTEN_ADDR` (default `:8080`). SIGTERM
cancels IDLE workers and releases DynamoDB leases.

```bash
./emailmcp -transport notify
# or, with docker compose (watcher on localhost:8081):
docker compose up emailmcp-notify
```

Watching starts for accounts that have a webhook URL and IMAP password. Empty
`watch_folders` means INBOX + the account's Sent folder (IMAP special-use).
Locally, omit `EMAILMCP_NOTIFY_STATE_TABLE` to use in-memory leases (not for
production). On ECS the table is required. MCPCICD env-stack deploys omit
`imageUri`; the notify image is a GetAtt of SSM `/email-mcp/notify/image-uri`
and stays the busybox placeholder until Dev Build or Prod Promote is followed
by writing that parameter (see the repo-root README pipeline contract). Do not
replace the bootstrap command with `-transport notify` while `entryPoint` is
`sh -c`.

The watcher has no write path for accounts, so it **requires the shared DynamoDB
user-config table** (`EMAILMCP_USER_CONFIG_TABLE`, also used by the HTTP server
and portal) and exits at startup if it is unset. With docker compose, set the
same `EMAILMCP_USER_CONFIG_TABLE` (and AWS credentials/region) in `emailmcp/.env`
for both `emailmcp` and `emailmcp-notify`; separate in-memory stores would never
share accounts. SSM lookups for the notify-state table happen only for the
notify transport.

### Installation (System-wide)

If you want to install EmailMCP to your system:

```bash
./run.sh install
```

This will:
- Build and install the binary to `/usr/local/bin/emailmcp-bin`
- Install a wrapper script to `/usr/local/bin/emailmcp`

The wrapper script automatically loads configuration from `~/.emailmcp`. You should create this file manually with your environment variables:

```bash
# Example ~/.emailmcp
EMAILMCP_USER_CONFIG_TABLE=EmailMCPUserConfigs
GOOGLE_CLIENT_ID=your-google-client-id.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=your-google-client-secret
PUBLIC_BASE_URL=https://emailmcp.ecg.co
APPLICATION_ID=default
```

### Cloud Deployment Configuration

When deploying to AWS, the following environment variables can be set in `emailmcp/.env` or your shell:

- `AWS_REGION`: The AWS region to deploy the infrastructure to (defaults to your AWS CLI configuration).
- `GOOGLE_CLIENT_ID`: Your Google OAuth2 Client ID for user authentication.
- `GOOGLE_CLIENT_SECRET`: Google OAuth client secret (HTTP/MCP OAuth flow).
- `PUBLIC_BASE_URL`: Public origin of the MCP server (ECS: `https://email.mcp.ecg.co` in Prod).

Cloud deploy and teardown are handled by MCPCICD, not this `run.sh`.

## MCP Tools

| Tool                  | Description                                              |
|-----------------------|----------------------------------------------------------|
| `add_email_account`   | Create/replace the user's IMAP + SMTP account (set passwords in the portal) |
| `list_email_accounts` | List the user's account (no secrets)                     |
| `remove_email_account`| Delete the user's account config                         |
| `list_folders`        | List IMAP mailboxes                                      |
| `search_emails`       | Search folder (summaries)                                |
| `read_email`          | Fetch full message by UID (attachment metadata includes id/part_id) |
| `get_attachment`      | Fetch one attachment (base64 or presigned URL)           |
| `move_emails`         | Bulk move UIDs                                           |
| `flag_emails`         | Add/remove flags                                         |
| `delete_emails`       | Mark emails deleted                                      |
| `send_email`          | Send now (text/HTML + data/url/path attachments, reply/forward) |
| `create_draft`        | Save to Drafts with `\\Draft`. Never sends.              |

Account tools require an `account_id` parameter to specify which email account to use.

## Configuration

EmailMCP uses **remote account storage only** (DynamoDB).

### Cloud Deployment Variables

When deploying the infrastructure, you must provide the following CDK context:

| Variable | Description |
|----------|-------------|
| `googleClientId` | Your Google OAuth2 Client ID for token verification. |

### Environment Variables

See `.env.example`. Key variables for the MCP server:

| Variable | Required | Description |
|----------|----------|-------------|
| `EMAILMCP_USER_CONFIG_TABLE` | No | DynamoDB table storing per-user email account configurations. Resolved from SSM `/emailmcp/user-config-table/name` when unset. Falls back to an in-memory store if neither is set. |
| `GOOGLE_CLIENT_ID` | Yes | Google OAuth2 Client ID for ID token verification. |
| `GOOGLE_CLIENT_SECRET` | Yes (HTTP) | Google client secret for the MCP OAuth authorize/token proxy. |
| `PUBLIC_BASE_URL` | Yes (HTTP) | Public origin (issuer + OAuth redirect base), e.g. `https://emailmcp.ecg.co`. |
| `EMAILMCP_SESSION_TABLE` | No | DynamoDB table storing OAuth sessions (opaque access/refresh tokens) and registered clients. Resolved from SSM `/emailmcp/session-table/name` when unset. Falls back to an in-memory store (sessions do not survive a restart or span replicas) if neither is set. |
| `OAUTH_REDIRECT_ALLOWLIST` | No | Comma-separated HTTPS OAuth `redirect_uri` allowlist (host, `*.host`, or `https://…` URI). **Empty = not enforced** (any HTTPS host allowed). Loopback HTTP and custom schemes always allowed. Passed to EKS via ConfigMap. |
| `APPLICATION_ID` | No | Application partition key (default: `default`). |
| `EMAILMCP_LOG_LEVEL` | No | `debug`, `info` (default), `warn`, `error`. |
| `EMAILMCP_TRANSPORT` | No | `http` (default), `stdio`, or `notify`. |
| `EMAILMCP_LISTEN_ADDR` | No | HTTP listen address (default: `:8080`). |
| `EMAILMCP_NOTIFY_STATE_TABLE` | No (local) / Yes (ECS) | DynamoDB table for notify leases and UID watermarks. Resolved from SSM `/emailmcp/notify-state-table/name` when unset. In-memory fallback is local-only. |
| `EMAILMCP_NOTIFY_OWNER` | No | Lease owner id. Must be unique per running task. Unset generates `hostname-pid-random`; the shared value `notify` is rejected. |
| `EMAILMCP_NOTIFY_RECONCILE` | No | Account reconcile interval (default `30s`, min `5s`, max `5m`). |
| `EMAILMCP_NOTIFY_WEBHOOK_TIMEOUT` | No | Webhook POST timeout (default `10s`, min `1s`, max `30s`). |
| `EMAILMCP_NOTIFY_MAX_FOLDERS` | No | Max watched folders per account (default `8`, hard cap `8`). |

### IMAP & SMTP Tuning
- `EMAILMCP_IMAP_MAX_CONNS`: Max connections per account (default: 4).
- `EMAILMCP_IMAP_IDLE_TIMEOUT`: Connection idle timeout (default: 5m).
- `EMAILMCP_SMTP_TIMEOUT`: SMTP operation timeout (default: 30s).

### Attachments, size limits, and Sent copies

| Variable | Default | Description |
|----------|---------|-------------|
| `EMAILMCP_ATTACHMENT_MAX_BYTES` | 10485760 (10 MiB) | Max decoded size per attachment (base64, URL, or path). |
| `EMAILMCP_MESSAGE_MAX_BYTES` | 26214400 (25 MiB) | Max encoded RFC822 size before send/draft. |
| `EMAILMCP_ATTACHMENT_URL_TIMEOUT` | 30s | HTTPS download timeout for `url` attachments. |
| `EMAILMCP_ATTACHMENT_ALLOWED_HOSTS` | empty | Optional HTTPS host allowlist for `url` attachments. Empty = any public host. |
| `EMAILMCP_ATTACHMENT_PATH_ALLOWLIST` | empty | Comma-separated directories for `path` attachments. Empty = path attachments disabled. |
| `EMAILMCP_ATTACHMENT_INLINE_MAX` | 1048576 (1 MiB) | `get_attachment` returns base64 at or under this size. |
| `EMAILMCP_ATTACHMENT_S3_BUCKET` | empty | When set, larger attachments get a short-lived presigned GET URL. |
| `EMAILMCP_ATTACHMENT_S3_PREFIX` | `emailmcp-attachments/` | S3 key prefix. |
| `EMAILMCP_ATTACHMENT_PRESIGN_TTL` | 15m | Presigned URL lifetime. |
| `EMAILMCP_SAVE_TO_SENT` | `auto` | After SMTP send, IMAP-APPEND to Sent (`auto` / `true` / `false`). |

**WorkMail notes** (AWS quotas as of 2026): incoming/outgoing MIME messages are capped at **40 MB** (29 MB unencoded). EmailMCP also enforces `EMAILMCP_MESSAGE_MAX_BYTES` (25 MiB by default); raise it up to 40 MiB if you need the full WorkMail cap. WorkMail SMTP does **not** reliably save a copy to Sent Items (unlike the web client). `save_to_sent=auto` therefore APPENDs a Sent copy for `*.awsapps.com` / WorkMail hosts, and skips the copy for Gmail (which already saves). Override per account (`save_to_sent` on `add_email_account`) or per `send_email` call.

Use `create_draft` when the user should review the message; it APPENDs to Drafts with `\\Draft` and never calls SMTP. Attach by `url` (HTTPS only, SSRF-blocked) for large files such as presigned S3 objects instead of inline base64.

## Docker

```bash
docker build -t emailmcp .
docker run \
  -e EMAILMCP_USER_CONFIG_TABLE=... \
  -e GOOGLE_CLIENT_ID=... \
  -e GOOGLE_CLIENT_SECRET=... \
  -e PUBLIC_BASE_URL=https://emailmcp.ecg.co \
  -e AWS_REGION=us-east-1 \
  -p 8080:8080 emailmcp
```

The container needs AWS credentials (or IRSA on EKS) to access DynamoDB.

## EKS Deployment

EmailMCP can be deployed to Amazon EKS using the provided Kubernetes manifests and the `run.sh` helper script.

### Prerequisites

1.  An existing EKS cluster.
2.  `kubectl` configured to point to your cluster.
3.  `aws` CLI configured with appropriate permissions.
4.  Infrastructure deployed via CDK (to create ECR and DynamoDB tables):
    ```bash
    ./run.sh deploy cloud
    ```

### Deployment Steps

1.  **Set Environment Variables**: Ensure `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, and optionally `PUBLIC_BASE_URL` are set in your environment or `.env` file. In Google Cloud Console, add authorized redirect URI `{PUBLIC_BASE_URL}/oauth/callback` (e.g. `https://emailmcp.ecg.co/oauth/callback`).
2.  **Deploy to EKS**:
    ```bash
    ./run.sh eks-deploy
    ```
    This command will:
    - Automatically detect your EKS cluster's OIDC Provider.
    - Provision an IAM Role for Service Accounts (IRSA) via CDK.
    - Build the Docker image and push it to the ECR repository.
    - Deploy a `ServiceAccount` annotated with the IAM Role.
    - Deploy the `Deployment` using the `ServiceAccount`, eliminating the need for static AWS Access Keys.
    - Deploy an AWS Application Load Balancer (ALB) via `Ingress` configured for `emailmcp.ecg.co`.

3.  **Verify Deployment**:
    ```bash
    ./run.sh eks-status
    ```

4.  **Access the Server**:
    The service is exposed via an AWS Application Load Balancer (ALB) at `http://emailmcp.ecg.co`. Ensure your DNS or `/etc/hosts` points `emailmcp.ecg.co` to the ALB's external DNS name (found in `eks-status`).

Account configuration is always remote (DynamoDB). The pod is stateless — no persistent volume is required for account data.

## Claude Desktop Integration

To use EmailMCP with Claude Desktop, add the following to your configuration file:

- **macOS**: `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Windows**: `%APPDATA%\Claude\claude_desktop_config.json`

If you have performed the [System-wide Installation](#installation-system-wide), the configuration is simple as the wrapper script handles the environment:

```json
{
  "mcpServers": {
    "emailmcp": {
      "command": "/usr/local/bin/emailmcp",
      "args": ["-transport", "stdio"]
    }
  }
}
```

Otherwise, you can run it from your source directory (requires absolute paths):

```json
{
  "mcpServers": {
    "emailmcp": {
      "command": "/absolute/path/to/EmailMCP/emailmcp",
      "args": ["-transport", "stdio"],
      "env": {
        "EMAILMCP_USER_CONFIG_TABLE": "EmailMCPUserConfigs",
        "GOOGLE_CLIENT_ID": "your-google-client-id.apps.googleusercontent.com"
      }
    }
  }
}
```

Note: HTTP mode with a Google ID token is the supported multi-user path. Stdio does not apply the HTTP auth middleware.

## Architecture

### System Diagram

The following diagram illustrates the interaction between components, highlighting the user and service authentication flows.

```mermaid
graph TD
    subgraph "Client Side"
        Client[AI Agent / Claude Desktop]
    end

    subgraph "Public Services"
        Google[Google OAuth2 <br/> ID Token Provider]
        EmailSvc[Email Provider <br/> IMAP/SMTP]
    end

    subgraph "AWS Infrastructure"
        MCP[EmailMCP Server <br/> Go]
        DDB[(DynamoDB <br/> User Configs)]
    end

    Client -- "1. MCP OAuth (or Bearer ID token)" --> MCP
    MCP -. "2. /oauth/authorize redirects user" .-> Google
    Google -. "3. /oauth/callback + ID token" .-> MCP
    Client -- "4. Bearer Google ID Token" --> MCP
    MCP -. "5. Validate Token" .-> Google
    MCP -- "6. Get Config (AWS SDK)" --> DDB
    MCP -- "7. IMAP/SMTP ops" --> EmailSvc

    %% Styling
    classDef highlight fill:#f9f,stroke:#333,stroke-width:2px;
    class MCP highlight;
```

### Cloud Components

EmailMCP is ready for cloud-native deployment with the following components:

- **Go MCP Server**: Validates Google ID tokens and dynamically fetches per-user configuration directly from DynamoDB.
- **DynamoDB Storage**: Stores user metadata and encrypted IMAP/SMTP credentials (encrypted at rest with KMS).
- **AWS CDK**: Defines and provisions all resources including KMS keys for encryption, IAM roles for least-privilege access, and CloudTrail for audit logging.

For more details on the deployment process, see the root-level `run.sh` script and the `infrastructure/` directory.

### Project Structure

```
emailmcp/
  cmd/emailmcp          - Entry point
  internal/
    config/             - Env config + DynamoDB store (SDK)
    imap/               - Connection pool + dedicated IDLE + operations (go-imap/v2)
    notify/             - IMAP IDLE supervisor, leases, webhook POST
    smtp/               - Sending logic (jordan-wright/email)
    server/             - MCP server, Google auth, tool registration
    types/              - Shared domain types
```

## Development

```bash
go test ./...
go vet ./...
go build ./...
```

## Security Notes

- Never log credentials or full email bodies in production.
- Account secrets live only in DynamoDB (encrypted); EmailMCP does not persist them.
- Use TLS for IMAP/SMTP in production.
- Consider running behind a reverse proxy if exposing publicly; MCP routes already require Google ID tokens.

## AWS Resource Cost Estimate

Based on the infrastructure provided in this project, here is an estimate of the AWS resource costs for a single user performing 4 email check-and-summarize cycles per hour (approx. 2,880 checks/month).

### 1. Fixed Infrastructure Costs (Monthly)
These costs are incurred once the infrastructure is deployed, regardless of usage volume.

| Resource | Description | Monthly Cost (Est.) |
| :--- | :--- | :--- |
| **Amazon EKS** | Control Plane ($0.10/hour) | $73.00 |
| **Application Load Balancer** | ALB Ingress ($0.0225/hour + LCU) | $16.50 |
| **AWS KMS** | 1 Customer Managed Key (CMK) | $1.00 |
| **Total Fixed Costs** | | **$90.50** |

### 2. Variable Usage Costs (Monthly)
These costs scale with the number of checks and summaries performed.

| Resource | Activity Detail | Monthly Cost (Est.) |
| :--- | :--- | :--- |
| **AWS Bedrock (LLM)** | Claude 3.5 Sonnet (2,880 summaries) | $25.92 |
| **AWS Fargate** | 0.5 vCPU / 1GB RAM for Go MCP Server | $18.02 |
| **Amazon DynamoDB** | ~5,760 Read/Write Units (On-demand) | < $0.01 |
| **Total Variable Costs** | | **$43.94** |

### 3. Total Cost Estimate

*   **Dedicated Environment (Single User)**: ~$134.44 / month
*   **Shared Environment (Incremental Cost)**: ~$43.94 / month

### 4. Cost Optimization Opportunities
*   **Lower-cost Model**: Switching to **Claude 3 Haiku** for summarization reduces Bedrock costs to ~$2.16/month.
*   **Lambda-based Hosting**: Adapting the Go MCP server to run on AWS Lambda or App Runner could eliminate the fixed EKS/ALB costs.
*   **Local Execution**: Running the MCP server locally and only calling DynamoDB and Bedrock costs ~$27.32/month.

*Note: Prices are based on `us-east-1` region. Bedrock costs assume 2,000 input tokens and 200 output tokens per summary.*
