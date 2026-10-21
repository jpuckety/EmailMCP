# Changelog

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
