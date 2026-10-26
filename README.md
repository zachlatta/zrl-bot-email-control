# zrl-bot email control

An MCP server that gives authorized AI agents full control of **one** Gmail mailbox
(default `zrl-bot@hackclub.com`) while hard-limiting who that mailbox can ever send to.

- **Agents can do anything with the mailbox:** search and read mail and threads, download
  attachments, archive, label, star, mark read/unread, trash, untrash, permanently delete,
  manage labels and filters, write and edit drafts, and send, reply, reply-all and forward
  (attachments included).
- **Outgoing mail can only reach `ALLOWED_RECIPIENTS`.** Every address in To, Cc and Bcc must
  be on the list, or nothing is sent. This is enforced in code, not by prompting.

## The outbound gate

`ALLOWED_RECIPIENTS` is read from the environment once at startup and cannot be changed through
MCP or the admin page. Enforcement is layered:

1. **Final-bytes check.** `internal/mailbox/gate.go` is the only code that calls Gmail's
   `messages.send`, `drafts.create` and `drafts.update`. Each call first runs
   `recipients.Allowlist.CheckRaw` on the exact RFC 5322 bytes handed to Gmail. The check
   parses every `To`, `Cc` and `Bcc` header, including repeated and folded ones. It rejects
   `Resent-*` headers, an empty recipient list, unparseable addresses, non-ASCII or control
   bytes, and bare CRs in the header block. Matching is exact and case-insensitive: no
   plus-address folding and no domain wildcards.
2. **Repository contract test.** `internal/mailbox/contract_test.go` fails the build if any other
   file calls those APIs, or if anything uses `drafts.send`, `messages.import/insert`,
   forwarding addresses, auto-forwarding, vacation responders, send-as aliases, delegates,
   IMAP/POP settings, SMTP or raw Gmail HTTP.
3. **Drafts.** Drafts are checked when they are created or updated. `send_draft` never calls
   Gmail's `drafts.send`. It downloads the draft's raw bytes, gates them, sends that exact copy
   with `messages.send`, and then deletes the draft. A draft edited in the Gmail UI to add an
   outside recipient is therefore refused.
4. **Filters.** A filter's auto-forward target must also be allowlisted.
5. **Account pinning.** Before any send, the server confirms the connected Google account is
   `MAILBOX_ADDRESS`. The Google connect flow also refuses any other account.
6. **Structured composition.** Agents never supply raw headers. Addresses must be bare
   addr-specs, and CR, LF or NUL in a subject, filename or threading header is rejected.

When `ALLOWED_RECIPIENTS` is empty, all sending is disabled.

## MCP tools

| Area | Tools |
| --- | --- |
| Read | `get_mailbox_info`, `search_messages`, `search_threads`, `get_message`, `get_thread`, `get_attachment` (images also come back as image content) |
| Send | `send_email`, `reply_to_message`, `forward_message` |
| Drafts | `create_draft`, `create_reply_draft`, `update_draft`, `list_drafts`, `get_draft`, `send_draft`, `delete_draft` |
| Organize | `modify_message_labels`, `modify_thread_labels`, `archive_messages`, `mark_read`, `mark_unread`, `trash_messages`, `untrash_messages`, `trash_thread`, `untrash_thread`, `delete_messages_permanently` |
| Labels and filters | `list_labels`, `create_label`, `update_label`, `delete_label`, `list_filters`, `create_filter`, `delete_filter` |

Search uses Gmail query syntax (`in:inbox is:unread has:attachment newer_than:7d`). Label
arguments accept IDs (`INBOX`, `UNREAD`, `STARRED`) or label names. Attachments can be sent as
base64 or by reference to an attachment already in the mailbox (`from_message_id` +
`attachment_id`). `forward_message` includes the original attachments by default.

- Every `date` and `last_date` is when Gmail received or sent the message, as an RFC 5322 date
  in America/New_York. Raw `Date` headers keep each sender's offset, so a thread read out of
  order; the raw header is still in `headers` with `include_headers`.
- `text` is optional when `html` is given. The plain-text part is then made from the HTML:
  links keep their URLs, list items become `- ` lines, and table cells are joined with ` | `.
  Message `text` in read results uses LF line endings.
- Send, reply, forward and `send_draft` results list the `to`, `cc`, `bcc` and `subject` read
  from the delivered bytes, so there is no need to fetch the sent message to check them.
- `reply_to_message` with `archive_thread` archives the thread after the reply is sent and sets
  `archived`. A blocked or failed send archives nothing. If the archive alone fails, the result
  carries `archive_error`; the reply was still sent and must not be resent.
- `get_thread` cuts each reply's bottom-posted quote of earlier messages (the `> ` lines and
  the `On ... wrote:` line) and sets `quoted_trimmed`. The first message is never cut, because
  it may quote mail from outside the thread. Pass `include_quoted` to keep everything.
- `get_thread` with `latest: N` returns only the newest N messages; `message_count` is always
  the whole thread. Threading headers (`Message-ID`, `In-Reply-To`, `References`) are left out
  of message details, because the server threads replies itself and `References` grows with
  every reply. They are still in `headers` with `include_headers`.
- `get_attachment` takes `offset` and `length` to download a large file in pieces. Follow
  `next_offset` until it is absent. Use 5 MiB pieces through a proxy such as PDW.
- `reply_to_message` answers the sender, or the `Reply-To` address. Replying to a message this
  mailbox sent goes to that message's `Reply-To`, else its `To`, so no `to` is needed to follow
  up on the bot's own email.
- `send_email` takes `reply_to` (allowlisted addresses). To file a note quietly in the bot's own
  inbox, send it to the bot (put the bot's address in `ALLOWED_RECIPIENTS`) with `reply_to` set to
  the human: nobody is notified, and a later `reply_to_message` on it reaches the human.
- `send_email` with `thread_id` sends a real reply to the thread's latest message: it sets
  `In-Reply-To` and `References`, and an empty subject becomes `Re: <thread subject>`. It still
  sends only to the addresses given. Use `reply_to_message` to answer whoever wrote.

## Authentication

There are two separate OAuth relationships:

- **Agents → this server.** OAuth 2.1 authorization code with S256 PKCE, dynamic client
  registration, and authorization-server and protected-resource metadata. The authorize page
  asks for `MCP_SECRET` and names the client and the host that will receive access. The page
  cannot be framed, and a wrong secret is answered slowly. Registration is open, as the MCP
  spec requires, so it is bounded: at most 10 redirect URIs per client and 200 clients in
  total. The oldest registration is dropped first, and its refresh tokens keep working.
  Refresh tokens last until revoked, so a client authorizes once. This is the same flow as
  `zachlatta/tasks`.
- **This server → Gmail.** A one-time Google consent from the admin page, signed in as the bot
  account. Scopes: `https://mail.google.com/` and `gmail.settings.basic`. The refresh token is
  persisted and refreshed automatically.

Clients, hashed codes and tokens, and the Google token are stored in `DATA_DIR/state.json`
(mode 0600). Mount `DATA_DIR` on a persistent volume.

### Connecting the mailbox

1. Open `https://<host>/admin` and log in with `MCP_SECRET`.
2. Click **Connect Gmail**, open the Google link, and sign in as the bot account. A private
   window helps.
3. With a Desktop ("installed") OAuth client, Google redirects to a `http://localhost/?state=…&code=…`
   page that fails to load. Paste that whole URL back into the admin page. With a Web client
   that lists `https://<host>/admin/google/callback`, the redirect completes on its own.
4. Use **Send test email** to confirm delivery to an allowlisted address.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `MCP_SECRET` | required, 24 or more characters | Admin login and approval of MCP clients |
| `ALLOWED_RECIPIENTS` | empty (sending disabled) | Comma-separated bare addresses the bot may send to |
| `MAILBOX_ADDRESS` | `zrl-bot@hackclub.com` | The only Google account the server will use |
| `PUBLIC_URL` | `http://<ADDR>` | Public origin; must be HTTPS unless loopback |
| `ADDR` | `127.0.0.1:8080` (`0.0.0.0:8080` in Docker) | Listen address |
| `DATA_DIR` | `data` (`/data` in Docker) | Persistent state |
| `GOOGLE_OAUTH_CLIENT_JSON` | none | Google OAuth client JSON, raw or base64 |
| `SOURCE_COMMIT` | `dev` | Version shown on `/healthz` (Coolify sets it) |

## Development

```sh
cp .env.example .env   # set MCP_SECRET
make test              # gofmt check, go vet, go test -race ./...
make run
```

The tests need no Google account. `internal/gmailfake` is an in-memory Gmail REST server. It
speaks the real wire protocol, including multipart media uploads, so the official
`google.golang.org/api/gmail/v1` client is exercised unmodified. Tests assert that blocked
messages never produce a send request.

## Deployment

The `Dockerfile` builds a static binary on Alpine and includes a `/healthz` health check. Run
it behind TLS, set the environment variables above, and mount a volume at `/data` that UID
10001 can write to.
