package mailbox

// This file is the only place in the repository allowed to call Gmail APIs
// that deliver mail or store a sendable draft. TestOutboundCallsOnlyInGate
// enforces that. Every path re-checks the exact bytes handed to Gmail against
// the recipient allowlist and verifies the connected account is the bot.

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

// deliver sends raw through messages.send after the hard allowlist check.
func (s *Service) deliver(ctx context.Context, service *gmail.Service, raw []byte, threadID string) (*gmail.Message, error) {
	if err := s.cfg.Allowlist.CheckRaw(raw); err != nil {
		return nil, err
	}
	if err := s.verifyMailbox(ctx, service); err != nil {
		return nil, err
	}
	return service.Users.Messages.Send("me", &gmail.Message{ThreadId: threadID}).
		Media(bytes.NewReader(raw), googleapi.ContentType("message/rfc822")).
		Context(ctx).Do()
}

// storeDraft creates (draftID == "") or replaces a draft after the same check,
// so no draft that could later be sent from the Gmail UI ever names an outside
// recipient through this server.
func (s *Service) storeDraft(ctx context.Context, service *gmail.Service, draftID string, raw []byte, threadID string) (*gmail.Draft, error) {
	if err := s.cfg.Allowlist.CheckRaw(raw); err != nil {
		return nil, err
	}
	if err := s.verifyMailbox(ctx, service); err != nil {
		return nil, err
	}
	draft := &gmail.Draft{Message: &gmail.Message{ThreadId: threadID}}
	media := googleapi.ContentType("message/rfc822")
	if draftID == "" {
		return service.Users.Drafts.Create("me", draft).Media(bytes.NewReader(raw), media).Context(ctx).Do()
	}
	draft.Id = draftID
	return service.Users.Drafts.Update("me", draftID, draft).Media(bytes.NewReader(raw), media).Context(ctx).Do()
}

// gateFilterForward rejects auto-forwarding filters to outside addresses.
func (s *Service) gateFilterForward(address string) error {
	if address == "" {
		return nil
	}
	return s.cfg.Allowlist.CheckAddresses([]string{address})
}

// verifyMailbox refuses to send from any account other than the configured
// bot address.
func (s *Service) verifyMailbox(ctx context.Context, service *gmail.Service) error {
	profile, err := service.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("verify mailbox: %w", err)
	}
	if !strings.EqualFold(profile.EmailAddress, s.cfg.Address) {
		return fmt.Errorf("connected Gmail account is %s, not %s; refusing to send", profile.EmailAddress, s.cfg.Address)
	}
	return nil
}
