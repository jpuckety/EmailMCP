# Drafts

Use `create_draft` when the user should review or finish the message before it is sent. Use `send_email` only when they want it sent immediately.

## `create_draft` never sends

`create_draft` does not use SMTP. It IMAP-APPENDs the complete message to Drafts with the `\Draft` flag and returns `folder`, `uid` (when the server provides it), and `message_id`.

## Same compose options as `send_email`

`create_draft` accepts the same body, recipients, attachments (`data` / `url` / `path`), reply, and forward fields as `send_email`:

- `to`, `cc`, `bcc`, `subject`, `text`, `html`, `from`, `attachments`
- `reply_to_uid`, `reply_folder`, `reply_all`, `quote_original`
- `forward_uid`, `forward_folder`, `include_original_attachments`

Follow `email://guides/replying`, `email://guides/forwarding`, and `email://guides/attachments` for those options.

## When to draft vs send

- User asked to draft, save, preview, or edit first → `create_draft`
- User asked to send now → `send_email`
- Unclear whether they want it sent → prefer `create_draft`

## Drafts folder

Omit `folder` unless the user named a mailbox. Default discovery is SPECIAL-USE `\Drafts`, then `Drafts` / `INBOX.Drafts`. Optional `folder` overrides that discovery.
