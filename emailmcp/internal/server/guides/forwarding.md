# Forwarding

Use this when the user wants to forward an existing message. Call `send_email` or `create_draft` with `forward_uid`.

## Required flow

1. Identify the original with `read_email` if you need its body or recipients.
2. Call `send_email` or `create_draft` with `forward_uid` set to that UID.
3. Set `forward_folder` when the original is not in INBOX. Empty/`forward_folder` omitted defaults to INBOX.
4. Always supply `to` (and optional `cc`/`bcc`). Forwarding does not fill recipients from the original.

## Subject and quoting

Omit `subject` unless you need to override it. It defaults to `Fwd:` plus the original subject (existing `Re:`/`Fwd:` prefixes are not stacked).

`quote_original` is **not** on by default. Set `quote_original: true` so the server appends a quoted copy of the original body below your new `text`/`html`.

Write only the new commentary in `text`/`html`. Do not paste the original yourself when `quote_original` is true.

## Original attachments

`include_original_attachments` applies **only when forwarding**. When omitted, it defaults to **true**: the server re-attaches non-inline parts from the original.

Inline / `cid:` parts are skipped. Set `include_original_attachments: false` to send the forward without those original files.

You may still add extra files in `attachments`; they are combined with any original attachments the server includes.
