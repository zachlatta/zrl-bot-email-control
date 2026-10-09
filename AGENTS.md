# Repository guidelines

## Development workflow

- This is a Go repository. Keep code formatted with `gofmt`.
- Use test-driven development for every behavior change. Write or update a test first and
  confirm it fails for the expected reason. Then implement the smallest change that passes,
  and refactor with the tests green.
- `make test` (gofmt check, `go vet`, `go test -race ./...`) is the full verification. Run it
  end to end before finishing, and also test the affected behavior by hand. Report both
  results.
- When asked to change an existing flow, replace it completely and remove the old code.

## The outbound gate is the product

- Only `internal/mailbox/gate.go` may call Gmail APIs that deliver mail or store sendable
  drafts. Each of those calls must sit behind `Allowlist.CheckRaw` on the final bytes. The
  contract tests in `internal/mailbox/contract_test.go` enforce this. Never weaken, skip or
  special-case them.
- Never add a tool or endpoint that accepts raw RFC 5322 from agents. Never add one that
  sends via `drafts.send`, or that touches forwarding addresses, auto-forwarding, vacation
  responders, send-as, delegates, IMAP/POP or SMTP. Any of these could reach people outside
  the allowlist.
- The allowlist comes only from `ALLOWED_RECIPIENTS` at startup. Do not make it editable at
  runtime.
- Every new outbound path needs a test that a stranger in To, Cc and Bcc is blocked, and that
  the fake Gmail received no send request.

## Public repository safety

- Everything committed here is public. Never commit secrets, OAuth client files, tokens,
  `.env`, `data/`, personal data or private URLs.
- Before committing, review the complete diff and staged file list.

## Commits

- Commit finished, verified work to `main` and push. Never push failing work.
