package mailbox

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/quotedprintable"
	"slices"
	"strings"
	"testing"

	"github.com/zachlatta/zrl-bot-email-control/internal/gmailfake"
	"github.com/zachlatta/zrl-bot-email-control/internal/recipients"
	"google.golang.org/api/gmail/v1"
)

const bot = "zrl-bot@hackclub.com"

func newService(t *testing.T) (*Service, *gmailfake.Server) {
	t.Helper()
	fake := gmailfake.New(t, bot)
	allow, err := recipients.ParseAllowlist("zach@hackclub.com,zach@zachlatta.com")
	if err != nil {
		t.Fatal(err)
	}
	service := New(Config{
		Address:   bot,
		Allowlist: allow,
		Gmail: func(context.Context) (*gmail.Service, error) {
			return fake.Service(t), nil
		},
	})
	return service, fake
}

func assertBlocked(t *testing.T, fake *gmailfake.Server, err error) {
	t.Helper()
	if !errors.Is(err, recipients.ErrRecipientNotAllowed) {
		t.Fatalf("err = %v, want ErrRecipientNotAllowed", err)
	}
	if sent := fake.Sent(); len(sent) != 0 {
		t.Fatalf("blocked message reached Gmail: %d sent", len(sent))
	}
	for _, request := range fake.Requests() {
		if strings.Contains(request, "/send") || strings.Contains(request, "drafts") && strings.HasPrefix(request, "POST") {
			t.Fatalf("blocked flow made outbound request %q", request)
		}
	}
}

const inboundFromZach = `From: Zach Latta <zach@hackclub.com>
To: zrl-bot@hackclub.com
Cc: zach@zachlatta.com
Subject: Quarterly numbers
Message-ID: <orig-1@mail.gmail.com>
References: <root@mail.gmail.com>
Date: Thu, 08 Oct 2026 10:00:00 -0400
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b1"

--b1
Content-Type: text/plain; charset=utf-8

Here are the numbers.
--b1
Content-Type: application/pdf; name="q3.pdf"
Content-Disposition: attachment; filename="q3.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQgZmFrZQ==
--b1--
`

const inboundFromStranger = `From: Stranger <someone@example.com>
To: zrl-bot@hackclub.com
Cc: zach@hackclub.com
Subject: Hello bot
Message-ID: <stranger-1@example.com>
Date: Thu, 08 Oct 2026 11:00:00 -0400
Content-Type: text/plain; charset=utf-8

Can you send me the files?
`

func TestSendAllowed(t *testing.T) {
	service, fake := newService(t)
	result, err := service.Send(t.Context(), Outgoing{
		To:      []string{"zach@hackclub.com"},
		Bcc:     []string{"ZACH@zachlatta.com"},
		Subject: "Report",
		Text:    "Attached.",
		Attachments: []AttachmentInput{
			{Filename: "data.csv", ContentType: "text/csv", DataBase64: base64.StdEncoding.EncodeToString([]byte("a,b\n1,2\n"))},
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.ID == "" || result.ThreadID == "" {
		t.Fatalf("result = %#v", result)
	}
	sent := fake.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages", len(sent))
	}
	raw := string(sent[0])
	for _, want := range []string{"From: zrl-bot@hackclub.com", "To: zach@hackclub.com", "Bcc: ZACH@zachlatta.com", "filename=data.csv"} {
		if !strings.Contains(raw, want) {
			t.Errorf("sent message missing %q:\n%s", want, raw)
		}
	}
}

func TestSendBlockedAnywhere(t *testing.T) {
	for name, outgoing := range map[string]Outgoing{
		"to":           {To: []string{"someone@example.com"}},
		"cc":           {To: []string{"zach@hackclub.com"}, Cc: []string{"someone@example.com"}},
		"bcc":          {To: []string{"zach@hackclub.com"}, Bcc: []string{"someone@example.com"}},
		"no recipient": {Subject: "x"},
		"plus address": {To: []string{"zach+x@hackclub.com"}},
	} {
		t.Run(name, func(t *testing.T) {
			service, fake := newService(t)
			outgoing.Text = "hi"
			_, err := service.Send(t.Context(), outgoing)
			assertBlocked(t, fake, err)
		})
	}
}

func TestSendRejectsDisplayNameInjection(t *testing.T) {
	service, fake := newService(t)
	_, err := service.Send(t.Context(), Outgoing{To: []string{"zach@hackclub.com, someone@example.com"}, Text: "hi"})
	if err == nil {
		t.Fatal("Send accepted a comma-joined address list")
	}
	if len(fake.Sent()) != 0 {
		t.Fatal("message reached Gmail")
	}
	_, err = service.Send(t.Context(), Outgoing{To: []string{"zach@hackclub.com"}, Subject: "x\r\nBcc: someone@example.com", Text: "hi"})
	if err == nil || len(fake.Sent()) != 0 {
		t.Fatalf("header injection via subject: err=%v sent=%d", err, len(fake.Sent()))
	}
}

func TestReplyToAllowedSender(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX", "UNREAD")
	original, _ := fake.Message(id)
	result, err := service.Reply(t.Context(), ReplyInput{MessageID: id, Text: "Thanks!"})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if result.ThreadID != original.ThreadID {
		t.Errorf("reply thread = %q, want %q", result.ThreadID, original.ThreadID)
	}
	raw := string(fake.Sent()[0])
	for _, want := range []string{"To: zach@hackclub.com", "Subject: Re: Quarterly numbers", "In-Reply-To: <orig-1@mail.gmail.com>", "References: <root@mail.gmail.com> <orig-1@mail.gmail.com>", "Thanks!", "> Here are the numbers."} {
		if !strings.Contains(raw, want) {
			t.Errorf("reply missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "Cc:") {
		t.Errorf("plain reply should not cc anyone:\n%s", raw)
	}
}

func TestReplyQuotesOriginalInHTML(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX")
	if _, err := service.Reply(t.Context(), ReplyInput{MessageID: id, Text: "Done.\n\nSecond <line>."}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(fake.Sent()[0]))))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw := string(decoded)
	for _, want := range []string{
		"text/html",
		"<div>Done.</div><div><br></div><div>Second &lt;line&gt;.</div>",
		`<div class="gmail_quote">`,
		`<blockquote class="gmail_quote"`,
		"<div>Here are the numbers.</div></blockquote>",
		"On Thu, Oct 8, 2026 at 10:00 AM Zach Latta &lt;zach@hackclub.com&gt; wrote:",
		"On Thu, Oct 8, 2026 at 10:00 AM Zach Latta <zach@hackclub.com> wrote:\r\n> Here are the numbers.",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("reply missing %q:\n%s", want, raw)
		}
	}
}

func TestReplyAllToAllowedParticipants(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX")
	if _, err := service.Reply(t.Context(), ReplyInput{MessageID: id, ReplyAll: true, Text: "ok"}); err != nil {
		t.Fatalf("Reply all: %v", err)
	}
	raw := string(fake.Sent()[0])
	if !strings.Contains(raw, "Cc: zach@zachlatta.com") || strings.Contains(raw, "zrl-bot@hackclub.com\r\nCc") {
		t.Errorf("reply-all recipients wrong:\n%s", raw)
	}
	if strings.Contains(strings.SplitN(raw, "\r\n\r\n", 2)[0], "To: zrl-bot") || strings.Contains(raw, "Cc: zrl-bot") {
		t.Errorf("reply-all included the bot itself:\n%s", raw)
	}
}

func TestReplyToStrangerBlocked(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromStranger, "INBOX")
	_, err := service.Reply(t.Context(), ReplyInput{MessageID: id, Text: "sure"})
	assertBlocked(t, fake, err)
	_, err = service.Reply(t.Context(), ReplyInput{MessageID: id, ReplyAll: true, Text: "sure"})
	assertBlocked(t, fake, err)
}

func TestReplyWithExplicitRecipientsOverride(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromStranger, "INBOX")
	if _, err := service.Reply(t.Context(), ReplyInput{MessageID: id, To: []string{"zach@hackclub.com"}, Text: "FYI"}); err != nil {
		t.Fatalf("Reply override: %v", err)
	}
	if raw := string(fake.Sent()[0]); !strings.Contains(raw, "To: zach@hackclub.com") || strings.Contains(raw, "someone@example.com\r\n") && strings.Contains(raw, "To: someone") {
		t.Errorf("override reply raw:\n%s", raw)
	}
}

func TestForwardIncludesAttachments(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromStranger, "INBOX")
	withAttachment := fake.AddMessage(inboundFromZach, "INBOX")
	if _, err := service.Forward(t.Context(), ForwardInput{MessageID: withAttachment, To: []string{"zach@zachlatta.com"}, Text: "see below"}); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	raw := string(fake.Sent()[0])
	for _, want := range []string{"Subject: Fwd: Quarterly numbers", "see below", "---------- Forwarded message ---------", "filename=q3.pdf", "JVBERi0xLjQgZmFrZQ=="} {
		if !strings.Contains(raw, want) {
			t.Errorf("forward missing %q:\n%s", want, raw)
		}
	}
	_, err := service.Forward(t.Context(), ForwardInput{MessageID: id, To: []string{"zach@hackclub.com"}, Bcc: []string{"someone@example.com"}})
	if !errors.Is(err, recipients.ErrRecipientNotAllowed) || len(fake.Sent()) != 1 {
		t.Fatalf("forward to stranger: err=%v sent=%d", err, len(fake.Sent()))
	}
}

func TestSendWithAttachmentFromExistingMessage(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX")
	detail, err := service.GetMessage(t.Context(), id, GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Attachments) != 1 {
		t.Fatalf("attachments = %#v", detail.Attachments)
	}
	_, err = service.Send(t.Context(), Outgoing{
		To: []string{"zach@hackclub.com"}, Subject: "again", Text: "here",
		Attachments: []AttachmentInput{{FromMessageID: id, AttachmentID: detail.Attachments[0].AttachmentID}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if raw := string(fake.Sent()[0]); !strings.Contains(raw, "filename=q3.pdf") || !strings.Contains(raw, "JVBERi0xLjQgZmFrZQ==") {
		t.Errorf("re-attached file missing:\n%s", raw)
	}
}

func TestReadSearchAndAttachments(t *testing.T) {
	service, fake := newService(t)
	fake.AddMessage(inboundFromStranger, "INBOX")
	id := fake.AddMessage(inboundFromZach, "INBOX", "UNREAD")
	result, err := service.Search(t.Context(), SearchInput{Query: "quarterly"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(result.Messages) != 1 || result.Messages[0].ID != id || result.Messages[0].Subject != "Quarterly numbers" || !strings.Contains(result.Messages[0].From, "zach@hackclub.com") {
		t.Fatalf("Search = %#v", result.Messages)
	}
	if !slices.Contains(result.Messages[0].LabelIDs, "UNREAD") {
		t.Errorf("labels = %v", result.Messages[0].LabelIDs)
	}
	detail, err := service.GetMessage(t.Context(), id, GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(detail.Text) != "Here are the numbers." || detail.MessageIDHeader != "<orig-1@mail.gmail.com>" || detail.Cc != "zach@zachlatta.com" {
		t.Fatalf("detail = %#v", detail)
	}
	attachment, err := service.GetAttachment(t.Context(), id, detail.Attachments[0].AttachmentID, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(attachment.DataBase64)
	if string(data) != "%PDF-1.4 fake" || attachment.Filename != "q3.pdf" || attachment.MimeType != "application/pdf" {
		t.Fatalf("attachment = %#v (%q)", attachment, data)
	}
	if _, err := service.GetAttachment(t.Context(), id, detail.Attachments[0].AttachmentID, 3); err == nil {
		t.Fatal("GetAttachment ignored max_bytes")
	}
	thread, err := service.GetThread(t.Context(), detail.ThreadID, GetOptions{})
	if err != nil || len(thread.Messages) != 1 {
		t.Fatalf("GetThread = %#v, %v", thread, err)
	}
	threads, err := service.SearchThreads(t.Context(), SearchInput{Query: "hello bot"})
	if err != nil || len(threads.Threads) != 1 || threads.Threads[0].Subject != "Hello bot" {
		t.Fatalf("SearchThreads = %#v, %v", threads, err)
	}
}

func TestOrganize(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX", "UNREAD")
	ctx := t.Context()

	label, err := service.CreateLabel(ctx, LabelInput{Name: "Finance/Reports"})
	if err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	if err := service.ModifyMessages(ctx, []string{id}, []string{"finance/reports", "STARRED"}, []string{"UNREAD"}); err != nil {
		t.Fatalf("ModifyMessages: %v", err)
	}
	message, _ := fake.Message(id)
	if !slices.Contains(message.LabelIDs, label.ID) || !slices.Contains(message.LabelIDs, "STARRED") || slices.Contains(message.LabelIDs, "UNREAD") {
		t.Fatalf("labels after modify = %v", message.LabelIDs)
	}
	if err := service.ModifyMessages(ctx, []string{id}, []string{"No Such Label"}, nil); err == nil {
		t.Fatal("unknown label name accepted")
	}
	if err := service.Archive(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	message, _ = fake.Message(id)
	if slices.Contains(message.LabelIDs, "INBOX") {
		t.Fatal("archive left INBOX")
	}
	if _, err := service.UpdateLabel(ctx, label.ID, LabelInput{Name: "Finance/Q3"}); err != nil {
		t.Fatal(err)
	}
	labels, _ := service.ListLabels(ctx)
	if !slices.ContainsFunc(labels, func(l Label) bool { return l.Name == "Finance/Q3" }) {
		t.Fatalf("labels = %#v", labels)
	}
	if err := service.Trash(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	message, _ = fake.Message(id)
	if !slices.Contains(message.LabelIDs, "TRASH") {
		t.Fatal("trash did not apply")
	}
	if err := service.Untrash(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if err := service.ModifyThread(ctx, message.ThreadID, []string{"INBOX"}, nil); err != nil {
		t.Fatal(err)
	}
	message, _ = fake.Message(id)
	if !slices.Contains(message.LabelIDs, "INBOX") || slices.Contains(message.LabelIDs, "TRASH") {
		t.Fatalf("after untrash+thread modify: %v", message.LabelIDs)
	}
	if err := service.DeleteLabel(ctx, label.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.DeletePermanently(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Message(id); ok {
		t.Fatal("message survived permanent delete")
	}
}

func TestDraftsAreGatedAndSendThroughTheGate(t *testing.T) {
	service, fake := newService(t)
	ctx := t.Context()

	_, err := service.CreateDraft(ctx, Outgoing{To: []string{"someone@example.com"}, Text: "x"})
	assertBlocked(t, fake, err)

	draft, err := service.CreateDraft(ctx, Outgoing{To: []string{"zach@hackclub.com"}, Subject: "draft", Text: "v1"})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	if _, err := service.UpdateDraft(ctx, draft.ID, Outgoing{To: []string{"zach@hackclub.com"}, Cc: []string{"someone@example.com"}, Text: "v2"}); !errors.Is(err, recipients.ErrRecipientNotAllowed) {
		t.Fatalf("UpdateDraft to stranger: %v", err)
	}
	if _, err := service.UpdateDraft(ctx, draft.ID, Outgoing{To: []string{"zach@hackclub.com"}, Subject: "draft", Text: "v2"}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	drafts, err := service.ListDrafts(ctx, 10)
	if err != nil || len(drafts) != 1 || drafts[0].Subject != "draft" {
		t.Fatalf("ListDrafts = %#v, %v", drafts, err)
	}
	got, err := service.GetDraft(ctx, draft.ID)
	if err != nil || !strings.Contains(got.Message.Text, "v2") {
		t.Fatalf("GetDraft = %#v, %v", got, err)
	}

	// Someone edits the draft in the Gmail UI to add an outside recipient.
	fake.SetDraftRaw(draft.ID, "To: zach@hackclub.com\nBcc: someone@example.com\nSubject: sneaky\n\nbody\n")
	_, err = service.SendDraft(ctx, draft.ID)
	if !errors.Is(err, recipients.ErrRecipientNotAllowed) || len(fake.Sent()) != 0 {
		t.Fatalf("edited draft to stranger: err=%v sent=%d", err, len(fake.Sent()))
	}
	if len(fake.DraftIDs()) != 1 {
		t.Fatal("blocked draft should be left in place")
	}

	fake.SetDraftRaw(draft.ID, "To: zach@hackclub.com\nSubject: fine\n\nbody\n")
	sent, err := service.SendDraft(ctx, draft.ID)
	if err != nil {
		t.Fatalf("SendDraft: %v", err)
	}
	if len(fake.Sent()) != 1 || sent.ID == "" {
		t.Fatalf("SendDraft sent %d", len(fake.Sent()))
	}
	if ids := fake.DraftIDs(); len(ids) != 0 {
		t.Fatalf("draft not removed after send: %v", ids)
	}

	other, _ := service.CreateDraft(ctx, Outgoing{To: []string{"zach@hackclub.com"}, Text: "x"})
	if err := service.DeleteDraft(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
}

func TestFiltersGateForwarding(t *testing.T) {
	service, fake := newService(t)
	ctx := t.Context()
	_, err := service.CreateFilter(ctx, FilterInput{From: "news@example.com", Forward: "someone@example.com"})
	if !errors.Is(err, recipients.ErrRecipientNotAllowed) || len(fake.Filters()) != 0 {
		t.Fatalf("forwarding filter to stranger: %v (%d filters)", err, len(fake.Filters()))
	}
	filter, err := service.CreateFilter(ctx, FilterInput{From: "news@example.com", AddLabels: []string{"STARRED"}, RemoveLabels: []string{"INBOX"}})
	if err != nil {
		t.Fatalf("CreateFilter: %v", err)
	}
	filters, err := service.ListFilters(ctx)
	if err != nil || len(filters) != 1 || filters[0].Criteria.From != "news@example.com" {
		t.Fatalf("ListFilters = %#v, %v", filters, err)
	}
	if err := service.DeleteFilter(ctx, filter.ID); err != nil {
		t.Fatal(err)
	}
}

func TestProfile(t *testing.T) {
	service, _ := newService(t)
	profile, err := service.Profile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if profile.EmailAddress != bot || strings.Join(profile.AllowedRecipients, ",") != "zach@hackclub.com,zach@zachlatta.com" {
		t.Fatalf("Profile = %#v", profile)
	}
}

func TestWrongMailboxIsRefused(t *testing.T) {
	fake := gmailfake.New(t, "zach@hackclub.com")
	allow, _ := recipients.ParseAllowlist("zach@hackclub.com")
	service := New(Config{Address: bot, Allowlist: allow, Gmail: func(context.Context) (*gmail.Service, error) { return fake.Service(t), nil }})
	if _, err := service.Send(t.Context(), Outgoing{To: []string{"zach@hackclub.com"}, Text: "x"}); err == nil || len(fake.Sent()) != 0 {
		t.Fatalf("sent from the wrong mailbox: %v", err)
	}
}

func TestAllowlistCheckedBeforeGmailIsContacted(t *testing.T) {
	allow, _ := recipients.ParseAllowlist("zach@hackclub.com")
	calls := 0
	service := New(Config{Address: bot, Allowlist: allow, Gmail: func(context.Context) (*gmail.Service, error) {
		calls++
		return nil, ErrNotConnected
	}})
	ctx := t.Context()
	stranger := Outgoing{To: []string{"zach@hackclub.com"}, Bcc: []string{"someone@example.com"}, Text: "x"}
	if _, err := service.Send(ctx, stranger); !errors.Is(err, recipients.ErrRecipientNotAllowed) {
		t.Errorf("Send = %v", err)
	}
	if _, err := service.CreateDraft(ctx, stranger); !errors.Is(err, recipients.ErrRecipientNotAllowed) {
		t.Errorf("CreateDraft = %v", err)
	}
	if _, err := service.Forward(ctx, ForwardInput{MessageID: "m", To: []string{"someone@example.com"}}); !errors.Is(err, recipients.ErrRecipientNotAllowed) {
		t.Errorf("Forward = %v", err)
	}
	if _, err := service.CreateFilter(ctx, FilterInput{Forward: "someone@example.com"}); !errors.Is(err, recipients.ErrRecipientNotAllowed) {
		t.Errorf("CreateFilter = %v", err)
	}
	if calls != 0 {
		t.Errorf("Gmail was contacted %d times for blocked requests", calls)
	}
}

func TestAttachmentIDsAreStableAcrossFetches(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX")
	first, _ := service.GetMessage(t.Context(), id, GetOptions{})
	second, _ := service.GetMessage(t.Context(), id, GetOptions{})
	if first.Attachments[0].AttachmentID != second.Attachments[0].AttachmentID {
		t.Fatalf("attachment IDs differ across fetches: %q vs %q", first.Attachments[0].AttachmentID, second.Attachments[0].AttachmentID)
	}
	// A raw Gmail ID from an earlier fetch still downloads.
	message := fake.Service(t)
	raw, _ := message.Users.Messages.Get("me", id).Do()
	var gmailID string
	walkParts(raw.Payload, func(p *gmail.MessagePart) {
		if p.Filename != "" {
			gmailID = p.Body.AttachmentId
		}
	})
	data, err := service.GetAttachment(t.Context(), id, gmailID, 0)
	if err != nil || data.Size != int64(len("%PDF-1.4 fake")) {
		t.Fatalf("raw-ID download = %#v, %v", data, err)
	}
}
