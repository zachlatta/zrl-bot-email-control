package mailbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/mail"
	"slices"
	"strings"

	"github.com/zachlatta/zrl-bot-email-control/internal/mimemsg"
	"google.golang.org/api/gmail/v1"
)

// AttachmentInput is a file to attach: either inline bytes or a reference to
// an attachment already in the mailbox.
type AttachmentInput struct {
	Filename      string `json:"filename,omitempty" jsonschema:"File name. Required with data_base64; defaults to the original name for references."`
	ContentType   string `json:"content_type,omitempty" jsonschema:"MIME type; guessed from the file name when omitted."`
	DataBase64    string `json:"data_base64,omitempty" jsonschema:"Standard base64 file content."`
	FromMessageID string `json:"from_message_id,omitempty" jsonschema:"Attach an existing attachment from this message ID..."`
	AttachmentID  string `json:"attachment_id,omitempty" jsonschema:"...identified by this attachment_id from get_message."`
}

// Outgoing is a new message.
type Outgoing struct {
	To          []string          `json:"to,omitempty" jsonschema:"Bare email addresses. Every To/Cc/Bcc address must be on the allowlist."`
	Cc          []string          `json:"cc,omitempty"`
	Bcc         []string          `json:"bcc,omitempty"`
	Subject     string            `json:"subject,omitempty"`
	Text        string            `json:"text,omitempty" jsonschema:"Plain-text body."`
	HTML        string            `json:"html,omitempty" jsonschema:"Optional HTML body (sent alongside the text body)."`
	Attachments []AttachmentInput `json:"attachments,omitempty"`
	ThreadID    string            `json:"thread_id,omitempty" jsonschema:"Optional Gmail thread ID to file the message into."`

	inReplyTo  string
	references string
}

// SendResult identifies a sent message.
type SendResult struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"thread_id"`
	LabelIDs []string `json:"label_ids,omitempty"`
}

func (s *Service) compose(ctx context.Context, service *gmail.Service, outgoing Outgoing) ([]byte, error) {
	// Fail fast with a readable error; the authoritative check runs again on
	// the composed bytes inside gate.go.
	all := slices.Concat(outgoing.To, outgoing.Cc, outgoing.Bcc)
	for _, address := range all {
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, address) {
			return nil, fmt.Errorf("%q is not a single bare email address", address)
		}
	}
	if err := s.cfg.Allowlist.CheckAddresses(all); err != nil {
		return nil, err
	}
	attachments, err := s.loadAttachments(ctx, service, outgoing.Attachments)
	if err != nil {
		return nil, err
	}
	return mimemsg.Compose(mimemsg.Message{
		From: s.cfg.Address, To: outgoing.To, Cc: outgoing.Cc, Bcc: outgoing.Bcc,
		Subject: outgoing.Subject, Text: outgoing.Text, HTML: outgoing.HTML,
		Attachments: attachments, InReplyTo: outgoing.inReplyTo, References: outgoing.references,
	})
}

func (s *Service) loadAttachments(ctx context.Context, service *gmail.Service, inputs []AttachmentInput) ([]mimemsg.Attachment, error) {
	var out []mimemsg.Attachment
	for i, input := range inputs {
		switch {
		case input.DataBase64 != "" && input.FromMessageID == "":
			if input.Filename == "" {
				return nil, fmt.Errorf("attachment %d: filename is required with data_base64", i)
			}
			data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(input.DataBase64), ""))
			if err != nil {
				return nil, fmt.Errorf("attachment %q: data_base64 is not valid base64: %w", input.Filename, err)
			}
			out = append(out, mimemsg.Attachment{Filename: input.Filename, ContentType: input.ContentType, Data: data})
		case input.DataBase64 == "" && input.FromMessageID != "" && input.AttachmentID != "":
			info, data, err := s.fetchAttachment(ctx, service, input.FromMessageID, input.AttachmentID, 0)
			if err != nil {
				return nil, fmt.Errorf("attachment %d: %w", i, err)
			}
			filename, contentType := info.Filename, info.MimeType
			if input.Filename != "" {
				filename = input.Filename
			}
			if input.ContentType != "" {
				contentType = input.ContentType
			}
			out = append(out, mimemsg.Attachment{Filename: filename, ContentType: contentType, Data: data})
		default:
			return nil, fmt.Errorf("attachment %d: give either data_base64 (with filename) or from_message_id with attachment_id", i)
		}
	}
	return out, nil
}

// Send composes and sends a new message.
func (s *Service) Send(ctx context.Context, outgoing Outgoing) (SendResult, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return SendResult{}, err
	}
	raw, err := s.compose(ctx, service, outgoing)
	if err != nil {
		return SendResult{}, err
	}
	sent, err := s.deliver(ctx, service, raw, outgoing.ThreadID)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{ID: sent.Id, ThreadID: sent.ThreadId, LabelIDs: sent.LabelIds}, nil
}

// ReplyInput replies within an existing thread.
type ReplyInput struct {
	MessageID       string            `json:"message_id" jsonschema:"ID of the message being replied to."`
	ReplyAll        bool              `json:"reply_all,omitempty" jsonschema:"Also include the original To and Cc recipients."`
	To              []string          `json:"to,omitempty" jsonschema:"Override the recipients instead of deriving them from the original."`
	Cc              []string          `json:"cc,omitempty"`
	Bcc             []string          `json:"bcc,omitempty"`
	Text            string            `json:"text,omitempty"`
	HTML            string            `json:"html,omitempty"`
	Attachments     []AttachmentInput `json:"attachments,omitempty"`
	OmitQuote       bool              `json:"omit_quote,omitempty" jsonschema:"Do not quote the original message below the reply."`
	SaveAsDraftOnly bool              `json:"-"`
}

// Reply sends a threaded reply.
func (s *Service) Reply(ctx context.Context, input ReplyInput) (SendResult, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return SendResult{}, err
	}
	outgoing, err := s.replyOutgoing(ctx, service, input)
	if err != nil {
		return SendResult{}, err
	}
	raw, err := s.compose(ctx, service, outgoing)
	if err != nil {
		return SendResult{}, err
	}
	sent, err := s.deliver(ctx, service, raw, outgoing.ThreadID)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{ID: sent.Id, ThreadID: sent.ThreadId, LabelIDs: sent.LabelIds}, nil
}

func (s *Service) replyOutgoing(ctx context.Context, service *gmail.Service, input ReplyInput) (Outgoing, error) {
	if input.MessageID == "" {
		return Outgoing{}, errors.New("message_id is required")
	}
	original, err := service.Users.Messages.Get("me", input.MessageID).Format("full").Context(ctx).Do()
	if err != nil {
		return Outgoing{}, err
	}
	headers := headerMap(original.Payload)
	outgoing := Outgoing{To: input.To, Cc: input.Cc, Bcc: input.Bcc, Text: input.Text, HTML: input.HTML, Attachments: input.Attachments, ThreadID: original.ThreadId}
	if len(input.To)+len(input.Cc)+len(input.Bcc) == 0 {
		from := addressesOf(headers["from"])
		switch {
		case len(from) == 1 && strings.EqualFold(from[0], s.cfg.Address):
			outgoing.To = addressesOf(headers["to"])
		case headers["reply-to"] != "":
			outgoing.To = addressesOf(headers["reply-to"])
		default:
			outgoing.To = from
		}
		if input.ReplyAll {
			outgoing.Cc = slices.Concat(addressesOf(headers["to"]), addressesOf(headers["cc"]))
		}
		outgoing.To = s.withoutSelf(dedupe(outgoing.To, nil))
		outgoing.Cc = s.withoutSelf(dedupe(outgoing.Cc, outgoing.To))
		if err := s.cfg.Allowlist.CheckAddresses(slices.Concat(outgoing.To, outgoing.Cc)); err != nil {
			return Outgoing{}, fmt.Errorf("%w; pass explicit to/cc to redirect the reply to allowed addresses", err)
		}
	}
	subject := decodeHeader(headers["subject"])
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	outgoing.Subject = subject
	messageID := strings.TrimSpace(headers["message-id"])
	outgoing.inReplyTo = messageID
	outgoing.references = strings.TrimSpace(strings.Join(strings.Fields(headers["references"]+" "+messageID), " "))
	if !input.OmitQuote {
		body := detail(original, GetOptions{IncludeHTML: true})
		attribution := fmt.Sprintf("On %s, %s wrote:", headers["date"], decodeHeader(headers["from"]))
		outgoing.Text = strings.TrimRight(input.Text, "\n") + "\n\n" + attribution + "\n" + quoteText(body.Text)
		if input.HTML != "" {
			quoted := body.HTML
			if quoted == "" {
				quoted = "<pre>" + html.EscapeString(body.Text) + "</pre>"
			}
			outgoing.HTML = input.HTML + `<br><div class="gmail_quote"><div>` + html.EscapeString(attribution) + `</div><blockquote style="margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex">` + quoted + `</blockquote></div>`
		}
	}
	return outgoing, nil
}

// ForwardInput forwards an existing message.
type ForwardInput struct {
	MessageID          string            `json:"message_id"`
	To                 []string          `json:"to" jsonschema:"Recipients; all must be on the allowlist."`
	Cc                 []string          `json:"cc,omitempty"`
	Bcc                []string          `json:"bcc,omitempty"`
	Text               string            `json:"text,omitempty" jsonschema:"Optional note above the forwarded message."`
	HTML               string            `json:"html,omitempty"`
	ExcludeAttachments bool              `json:"exclude_attachments,omitempty" jsonschema:"Do not include the original attachments."`
	Attachments        []AttachmentInput `json:"attachments,omitempty" jsonschema:"Extra attachments to add."`
}

// Forward forwards a message (with its attachments by default).
func (s *Service) Forward(ctx context.Context, input ForwardInput) (SendResult, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return SendResult{}, err
	}
	if err := s.cfg.Allowlist.CheckAddresses(slices.Concat(input.To, input.Cc, input.Bcc)); err != nil {
		return SendResult{}, err
	}
	original, err := service.Users.Messages.Get("me", input.MessageID).Format("full").Context(ctx).Do()
	if err != nil {
		return SendResult{}, err
	}
	headers := headerMap(original.Payload)
	body := detail(original, GetOptions{IncludeHTML: true})
	subject := decodeHeader(headers["subject"])
	if !strings.HasPrefix(strings.ToLower(subject), "fwd:") {
		subject = "Fwd: " + subject
	}
	var meta strings.Builder
	meta.WriteString("---------- Forwarded message ---------\n")
	for _, field := range []struct{ name, key string }{{"From", "from"}, {"Date", "date"}, {"Subject", "subject"}, {"To", "to"}, {"Cc", "cc"}} {
		if value := decodeHeader(headers[field.key]); value != "" {
			fmt.Fprintf(&meta, "%s: %s\n", field.name, value)
		}
	}
	outgoing := Outgoing{To: input.To, Cc: input.Cc, Bcc: input.Bcc, Subject: subject}
	outgoing.Text = strings.TrimSpace(input.Text+"\n\n") + "\n\n" + meta.String() + "\n" + body.Text
	if input.HTML != "" || body.HTML != "" {
		note := input.HTML
		if note == "" {
			note = "<div>" + strings.ReplaceAll(html.EscapeString(input.Text), "\n", "<br>") + "</div>"
		}
		originalHTML := body.HTML
		if originalHTML == "" {
			originalHTML = "<pre>" + html.EscapeString(body.Text) + "</pre>"
		}
		outgoing.HTML = note + `<br><div class="gmail_quote">` + strings.ReplaceAll(html.EscapeString(meta.String()), "\n", "<br>") + "<br>" + originalHTML + "</div>"
	}
	if !input.ExcludeAttachments {
		for _, attachment := range body.Attachments {
			outgoing.Attachments = append(outgoing.Attachments, AttachmentInput{FromMessageID: original.Id, AttachmentID: attachment.AttachmentID})
		}
	}
	outgoing.Attachments = append(outgoing.Attachments, input.Attachments...)
	raw, err := s.compose(ctx, service, outgoing)
	if err != nil {
		return SendResult{}, err
	}
	sent, err := s.deliver(ctx, service, raw, "")
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{ID: sent.Id, ThreadID: sent.ThreadId, LabelIDs: sent.LabelIds}, nil
}

// DraftSummary lists a draft.
type DraftSummary struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Snippet   string `json:"snippet"`
}

// Draft is a full draft.
type Draft struct {
	ID      string        `json:"id"`
	Message MessageDetail `json:"message"`
}

func (s *Service) CreateDraft(ctx context.Context, outgoing Outgoing) (DraftSummary, error) {
	return s.saveDraft(ctx, "", outgoing)
}

func (s *Service) UpdateDraft(ctx context.Context, id string, outgoing Outgoing) (DraftSummary, error) {
	if id == "" {
		return DraftSummary{}, errors.New("draft_id is required")
	}
	return s.saveDraft(ctx, id, outgoing)
}

// CreateReplyDraft stores a threaded reply as a draft without sending it.
func (s *Service) CreateReplyDraft(ctx context.Context, input ReplyInput) (DraftSummary, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return DraftSummary{}, err
	}
	outgoing, err := s.replyOutgoing(ctx, service, input)
	if err != nil {
		return DraftSummary{}, err
	}
	return s.saveDraftWith(ctx, service, "", outgoing)
}

func (s *Service) saveDraft(ctx context.Context, id string, outgoing Outgoing) (DraftSummary, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return DraftSummary{}, err
	}
	return s.saveDraftWith(ctx, service, id, outgoing)
}

func (s *Service) saveDraftWith(ctx context.Context, service *gmail.Service, id string, outgoing Outgoing) (DraftSummary, error) {
	raw, err := s.compose(ctx, service, outgoing)
	if err != nil {
		return DraftSummary{}, err
	}
	draft, err := s.storeDraft(ctx, service, id, raw, outgoing.ThreadID)
	if err != nil {
		return DraftSummary{}, err
	}
	summary := DraftSummary{ID: draft.Id, To: strings.Join(outgoing.To, ", "), Subject: outgoing.Subject}
	if draft.Message != nil {
		summary.MessageID, summary.ThreadID = draft.Message.Id, draft.Message.ThreadId
	}
	return summary, nil
}

func (s *Service) ListDrafts(ctx context.Context, maxResults int64) ([]DraftSummary, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return nil, err
	}
	response, err := service.Users.Drafts.List("me").MaxResults(clampResults(maxResults)).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	drafts := []DraftSummary{}
	for _, item := range response.Drafts {
		draft, err := service.Users.Drafts.Get("me", item.Id).Format("full").Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		summary := DraftSummary{ID: draft.Id}
		if draft.Message != nil {
			message := summarize(draft.Message)
			summary.MessageID, summary.ThreadID, summary.To, summary.Subject, summary.Snippet = message.ID, message.ThreadID, message.To, message.Subject, message.Snippet
		}
		drafts = append(drafts, summary)
	}
	return drafts, nil
}

func (s *Service) GetDraft(ctx context.Context, id string) (Draft, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return Draft{}, err
	}
	draft, err := service.Users.Drafts.Get("me", id).Format("full").Context(ctx).Do()
	if err != nil {
		return Draft{}, err
	}
	out := Draft{ID: draft.Id}
	if draft.Message != nil {
		out.Message = detail(draft.Message, GetOptions{IncludeHTML: true})
	}
	return out, nil
}

// SendDraft sends a draft's current content. It does not call drafts.send:
// it fetches the raw bytes, gates them, sends that exact copy with
// messages.send, and then deletes the draft. Whatever Gmail delivers is
// therefore precisely what was checked.
func (s *Service) SendDraft(ctx context.Context, id string) (SendResult, error) {
	service, err := s.gmail(ctx)
	if err != nil {
		return SendResult{}, err
	}
	draft, err := service.Users.Drafts.Get("me", id).Format("raw").Context(ctx).Do()
	if err != nil {
		return SendResult{}, err
	}
	if draft.Message == nil || draft.Message.Raw == "" {
		return SendResult{}, fmt.Errorf("draft %s has no content", id)
	}
	raw, err := decodeBase64URL(draft.Message.Raw)
	if err != nil {
		return SendResult{}, fmt.Errorf("decode draft: %w", err)
	}
	sent, err := s.deliver(ctx, service, raw, draft.Message.ThreadId)
	if err != nil {
		return SendResult{}, err
	}
	if err := service.Users.Drafts.Delete("me", id).Context(ctx).Do(); err != nil {
		return SendResult{ID: sent.Id, ThreadID: sent.ThreadId}, fmt.Errorf("sent as %s but could not delete draft %s: %w", sent.Id, id, err)
	}
	return SendResult{ID: sent.Id, ThreadID: sent.ThreadId, LabelIDs: sent.LabelIds}, nil
}

func (s *Service) DeleteDraft(ctx context.Context, id string) error {
	service, err := s.gmail(ctx)
	if err != nil {
		return err
	}
	return service.Users.Drafts.Delete("me", id).Context(ctx).Do()
}

func addressesOf(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	list, err := (&mail.AddressParser{WordDecoder: nil}).ParseList(value)
	if err != nil {
		// Unparseable recipients become a literal that the allowlist rejects.
		return []string{value}
	}
	out := make([]string, 0, len(list))
	for _, address := range list {
		out = append(out, address.Address)
	}
	return out
}

func dedupe(values, exclude []string) []string {
	var out []string
	for _, value := range values {
		if slices.ContainsFunc(out, func(v string) bool { return strings.EqualFold(v, value) }) ||
			slices.ContainsFunc(exclude, func(v string) bool { return strings.EqualFold(v, value) }) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func (s *Service) withoutSelf(values []string) []string {
	return slices.DeleteFunc(values, func(v string) bool { return strings.EqualFold(v, s.cfg.Address) })
}

func quoteText(text string) string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + line
		}
	}
	return strings.Join(lines, "\n")
}
