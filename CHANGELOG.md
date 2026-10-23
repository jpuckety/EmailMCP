# Changelog

## Unreleased

### Fixed
- Forwards and quoted replies with a text-only note now include that note in the HTML part, and quoted original HTML is the inner body (not a nested full document).

### Added
- Notify watcher logs IMAP IDLE start/refresh, each new mailbox message, and webhook delivery results (status, attempt, duration, host).
- Notify reconcile logs each watch gate: skipped accounts (with reason), folders selected, lease not acquired, and worker started.
- IMAP/SMTP passwords are encrypted with the customer-managed KMS key before DynamoDB writes; existing plaintext rows remain readable and are encrypted on their next update.
- MCP skill resources at `email://guides/replying`, `email://guides/forwarding`, `email://guides/attachments`, and `email://guides/drafts`. Initialize instructions and `send_email` / `create_draft` / `read_email` / `get_attachment` descriptions point at those URIs.
- `list_resources` and `read_resource` MCP tools that return the same catalog and markdown as `resources/list` / `resources/read`, so tool-only clients can load the guides. Initialize instructions and related tool descriptions tell agents to call `read_resource` with the guide URI.

## 0.3.2 — 2026-10-22

### Fixed
- MCP Streamable HTTP is now stateless, so concurrent tool calls no longer share a session keyed by JSON-RPC id (parallel searches can no longer return the other account's mailbox).
- `read_email` returns decoded text/html bodies instead of the raw MIME payload (boundaries and base64 attachments).
- Attachment `size` on `read_email` is the decoded byte count, matching `get_attachment`.
- Outgoing `Message-ID` / `In-Reply-To` / `References` are always written with angle brackets.

## 0.3.1 — 2026-10-22

### Fixed
- IMAP pool no longer holds the account lock during NOOP/dial/login, so one slow mailbox cannot 504 every other request.
- Stale IMAP connections are health-checked with a timeout and operations retry once on EOF/reset.
- `send_email` returns after SMTP success; Sent-folder APPEND runs in the background so Gmail/WorkMail sends are not gated on IMAP.
- MCP Streamable HTTP uses JSON responses so concurrent tool calls on one session cannot receive each other's results.
- SMTP I/O is bound to the request deadline so a timed-out send cannot keep running and invite duplicate retries.
- ALB idle timeout raised to 120s to match the HTTP write timeout.

## 0.3.0 — 2026-10-21

### Added
- IMAP IDLE push notifications: one HTTPS webhook per account, optional Bearer token, and watch folders (empty list = INBOX + special-use Sent).
- Portal account form fields for webhook URL, write-only token (with remove-saved-token), and folders. Token is never returned on GET.
- `email-mcp-notify` Fargate service (`-transport notify`) with a KMS-encrypted `NotifyState` table for leases and UID watermarks. Notify image is SSM `/email-mcp/notify/image-uri` so env-stack deploys do not reset a promoted digest.

## 0.2.0 — 2026-10-21

### Added
- `create_draft` MCP tool: builds the same MIME message as `send_email` and IMAP-APPENDs it to Drafts with `\Draft`. Never sends.
- `get_attachment` MCP tool: fetch one attachment by `attachment_id`/`part_id` or `filename`. Returns base64 under the inline cap, or a short-lived S3 presigned URL when a bucket is configured.
- Attachment sources besides inline base64: HTTPS `url` (SSRF-protected) and optional allowlisted `path`.
- Optional `content_id` + `disposition=inline` for CID images (`<img src="cid:...">`).
- Reply (`reply_to_uid`, `reply_all`) and forward (`forward_uid`, `include_original_attachments`) on `send_email` and `create_draft`.
- `send_email` returns `message_id`, accepted/rejected recipients, and optional Sent-folder copy.

### Changed
- Shared MIME builder (`internal/mailbuild`) used by both send and draft.
- `read_email` attachment metadata now includes `id`/`part_id`, size, disposition, `content_id`, and `inline`.

### Configuration
- New env vars: `EMAILMCP_ATTACHMENT_MAX_BYTES`, `EMAILMCP_MESSAGE_MAX_BYTES`, `EMAILMCP_ATTACHMENT_URL_TIMEOUT`, `EMAILMCP_ATTACHMENT_ALLOWED_HOSTS`, `EMAILMCP_ATTACHMENT_PATH_ALLOWLIST`, `EMAILMCP_ATTACHMENT_INLINE_MAX`, `EMAILMCP_ATTACHMENT_S3_BUCKET`, `EMAILMCP_ATTACHMENT_S3_PREFIX`, `EMAILMCP_ATTACHMENT_PRESIGN_TTL`, `EMAILMCP_SAVE_TO_SENT`.

### WorkMail
- Provider MIME limit documented as 40 MB (29 MB unencoded). Default app cap remains 25 MiB.
- `save_to_sent=auto` APPENDs a Sent copy for WorkMail (SMTP does not reliably save) and skips Gmail.
