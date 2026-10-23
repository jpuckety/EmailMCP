# Replying

Use this when the user wants to reply to an existing message. Read the original with `read_email`, then call `send_email` or `create_draft` with `reply_to_uid`.

## Required flow

1. Call `read_email` with the original `uid` (and `folder` if it is not INBOX).
2. Compose the reply with `send_email` (send now) or `create_draft` (review first).
3. Set `reply_to_uid` to that UID.
4. Set `reply_folder` when the original is not in INBOX. Empty/`reply_folder` omitted defaults to INBOX.

## Threading and recipients

`reply_to_uid` makes the server set `In-Reply-To` and `References` from the original Message-ID.

Omit `to` and `subject` unless you need to override them:

- Subject defaults to `Re:` plus the original subject (existing `Re:`/`Fwd:` prefixes are not stacked).
- `to` defaults to the original `Reply-To` header if present, otherwise `From`. The account’s own addresses are omitted.

If you supply `to` or `cc`, the server keeps those values and does not auto-fill recipients.

Set `reply_all: true` to also copy the original To/Cc into `cc`, minus the account’s own address and the reply `to`. This only applies when you did not supply `to` or `cc`.

## Quoting

`quote_original` is **not** on by default. Set `quote_original: true` so the server appends a quoted copy of the original body below your new `text`/`html` (plain `>` quoting and an HTML blockquote).

Write only the new reply in `text`/`html`. Do not paste the original yourself when `quote_original` is true.

## Attachments

The server does not copy files from the original message on reply. To add new files, pass them in `attachments` as you would for a new message.
