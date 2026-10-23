# Attachments

`read_email` does not return attachment bytes. Fetch content with `get_attachment`. When composing, each outgoing item supplies exactly one source.

## Reading metadata

`read_email` lists attachments as metadata only:

- `id` / `part_id` — use either with `get_attachment` (`id` equals `part_id`)
- `filename`, `size`, `content_type`
- `inline`, `content_id`, `disposition`

## Fetching content

Call `get_attachment` with `account_id`, `uid`, and one of:

- `attachment_id` (preferred; same as `part_id`)
- `part_id`
- `filename`

`folder` defaults to INBOX.

The result is:

- `data` — base64, when the part is at or under the inline size cap (1 MiB unless configured otherwise)
- `url` — a short-lived presigned download URL, when over the cap and S3 is configured

If the part is over the cap and S3 is not configured, the call fails. Do not assume bytes are always inline.

## Sending

On `send_email` or `create_draft`, each `attachments[]` item must provide **exactly one** of:

- `data` — base64 bytes
- `url` — HTTPS URL (for example a presigned link from `get_attachment`)
- `path` — local filesystem path (**disabled** unless the server has a path allowlist)

Optional: `filename`, `content_type`. Disposition defaults to `attachment`.

## Inline `cid:` images

For images referenced from HTML as `cid:…`:

1. Set `disposition` to `inline`.
2. Set `content_id` to the CID (with or without angle brackets).
3. Reference it from `html` as `cid:<that-id>` (for example `<img src="cid:logo">`).

Path attachments fail unless the path is under the configured allowlist. Prefer `data` or `url`.
