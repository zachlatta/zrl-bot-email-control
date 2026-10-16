package mailbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

func TestSendInlineImage(t *testing.T) {
	service, fake := newService(t)
	_, err := service.Send(t.Context(), Outgoing{
		To: []string{"zach@hackclub.com"}, Subject: "photo", Text: "see photo",
		HTML: `<p>see photo</p><img src="cid:photo1">`,
		Attachments: []AttachmentInput{
			{Filename: "photo.jpg", ContentType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString([]byte("jpeg")), ContentID: "photo1"},
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw := string(fake.Sent()[0])
	for _, want := range []string{"multipart/related", "Content-Id: <photo1>", "Content-Disposition: inline"} {
		if !strings.Contains(raw, want) {
			t.Errorf("sent message missing %q:\n%s", want, raw)
		}
	}
}

func TestSendInlineImageStillGated(t *testing.T) {
	service, fake := newService(t)
	_, err := service.Send(t.Context(), Outgoing{
		To: []string{"zach@hackclub.com"}, Bcc: []string{"someone@example.com"}, Text: "x", HTML: `<img src="cid:a">`,
		Attachments: []AttachmentInput{{Filename: "a.png", DataBase64: base64.StdEncoding.EncodeToString([]byte("png")), ContentID: "a"}},
	})
	assertBlocked(t, fake, err)
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
	if strings.TrimSpace(detail.Text) != "Here are the numbers." || detail.Cc != "zach@zachlatta.com" {
		t.Fatalf("detail = %#v", detail)
	}
	attachment, err := service.GetAttachment(t.Context(), AttachmentRequest{MessageID: id, AttachmentID: detail.Attachments[0].AttachmentID})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(attachment.DataBase64)
	if string(data) != "%PDF-1.4 fake" || attachment.Filename != "q3.pdf" || attachment.MimeType != "application/pdf" {
		t.Fatalf("attachment = %#v (%q)", attachment, data)
	}
	if _, err := service.GetAttachment(t.Context(), AttachmentRequest{MessageID: id, AttachmentID: detail.Attachments[0].AttachmentID, MaxBytes: 3}); err == nil {
		t.Fatal("GetAttachment ignored max_bytes")
	}
	thread, err := service.GetThread(t.Context(), detail.ThreadID, ThreadOptions{})
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
	data, err := service.GetAttachment(t.Context(), AttachmentRequest{MessageID: id, AttachmentID: gmailID})
	if err != nil || data.Size != int64(len("%PDF-1.4 fake")) {
		t.Fatalf("raw-ID download = %#v, %v", data, err)
	}
}

const zachFollowUp = `From: Zach Latta <zach@hackclub.com>
To: zrl-bot@hackclub.com
Subject: Re: Quarterly numbers
Message-ID: <orig-2@mail.gmail.com>
In-Reply-To: <orig-1@mail.gmail.com>
References: <root@mail.gmail.com> <orig-1@mail.gmail.com>
Date: Thu, 08 Oct 2026 12:00:00 -0400
Content-Type: text/plain; charset=utf-8

Any update?

On Thu, Oct 8, 2026 at 10:00 AM Zach Latta <zach@hackclub.com>
wrote:
> Here are the numbers.
>
> Thanks
`

func TestSendIntoThreadRepliesToLatestMessage(t *testing.T) {
	service, fake := newService(t)
	first := fake.AddMessage(inboundFromZach, "INBOX")
	original, _ := fake.Message(first)
	fake.AddMessageInThread(original.ThreadID, zachFollowUp, "INBOX")

	result, err := service.Send(t.Context(), Outgoing{To: []string{"zach@hackclub.com"}, Text: "Update: done.", ThreadID: original.ThreadID})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.ThreadID != original.ThreadID {
		t.Errorf("thread = %q, want %q", result.ThreadID, original.ThreadID)
	}
	raw := string(fake.Sent()[0])
	for _, want := range []string{"Subject: Re: Quarterly numbers\r\n", "In-Reply-To: <orig-2@mail.gmail.com>", "References: <root@mail.gmail.com> <orig-1@mail.gmail.com> <orig-2@mail.gmail.com>"} {
		if !strings.Contains(raw, want) {
			t.Errorf("threaded send missing %q:\n%s", want, raw)
		}
	}

	if _, err := service.Send(t.Context(), Outgoing{To: []string{"zach@hackclub.com"}, Subject: "New subject", Text: "x", ThreadID: original.ThreadID}); err != nil {
		t.Fatal(err)
	}
	// The first update is now the thread's latest message, so the second
	// replies to it.
	firstUpdateID := strings.SplitN(strings.SplitN(raw, "Message-ID: ", 2)[1], "\r\n", 2)[0]
	if raw := string(fake.Sent()[1]); !strings.Contains(raw, "Subject: New subject\r\n") || !strings.Contains(raw, "In-Reply-To: "+firstUpdateID) || !strings.Contains(raw, "<orig-2@mail.gmail.com> "+firstUpdateID) {
		t.Errorf("explicit subject not kept or not threaded after %s:\n%s", firstUpdateID, raw)
	}

	for name, outgoing := range map[string]Outgoing{
		"to":  {To: []string{"someone@example.com"}},
		"cc":  {To: []string{"zach@hackclub.com"}, Cc: []string{"someone@example.com"}},
		"bcc": {To: []string{"zach@hackclub.com"}, Bcc: []string{"someone@example.com"}},
	} {
		t.Run(name, func(t *testing.T) {
			service, fake := newService(t)
			id := fake.AddMessage(inboundFromZach, "INBOX")
			message, _ := fake.Message(id)
			outgoing.ThreadID, outgoing.Text = message.ThreadID, "hi"
			_, err := service.Send(t.Context(), outgoing)
			assertBlocked(t, fake, err)
		})
	}
}

func TestGetThreadTrimsQuotedHistory(t *testing.T) {
	service, fake := newService(t)
	first := fake.AddMessage(inboundFromZach, "INBOX")
	original, _ := fake.Message(first)
	fake.AddMessageInThread(original.ThreadID, zachFollowUp, "INBOX")

	thread, err := service.GetThread(t.Context(), original.ThreadID, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(thread.Messages[1].Text); got != "Any update?" || !thread.Messages[1].QuotedTrimmed {
		t.Errorf("follow-up text = %q (trimmed=%v), want only the new text", got, thread.Messages[1].QuotedTrimmed)
	}
	if got := strings.TrimSpace(thread.Messages[0].Text); got != "Here are the numbers." || thread.Messages[0].QuotedTrimmed {
		t.Errorf("first message changed: %q", got)
	}

	full, err := service.GetThread(t.Context(), original.ThreadID, ThreadOptions{IncludeQuoted: true})
	if err != nil {
		t.Fatal(err)
	}
	if text := full.Messages[1].Text; !strings.Contains(text, "> Here are the numbers.") || full.Messages[1].QuotedTrimmed {
		t.Errorf("include_quoted dropped the quote: %q", text)
	}
}

func TestTrimQuotedKeepsInlineReplies(t *testing.T) {
	for name, text := range map[string]string{
		"inline answers": "> question one\nanswer one\n> question two\nanswer two\n",
		"no quote":       "Just text.\n",
		"only a quote":   "> all quoted\n",
	} {
		got, trimmed := trimQuoted(text)
		if trimmed || got != text {
			t.Errorf("%s: trimQuoted changed %q to %q", name, text, got)
		}
	}
	got, trimmed := trimQuoted("Sounds good.\n\nOn Fri, Oct 9, 2026 at 9:00 AM Bot <zrl-bot@hackclub.com> wrote:\n\n> earlier\n> more\n\n")
	if !trimmed || got != "Sounds good." {
		t.Errorf("trimQuoted = %q, %v", got, trimmed)
	}
}

func TestGetAttachmentInChunks(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX")
	detail, _ := service.GetMessage(t.Context(), id, GetOptions{})
	attachmentID := detail.Attachments[0].AttachmentID
	const whole = "%PDF-1.4 fake"

	var got []byte
	offset := int64(0)
	for range 10 {
		chunk, err := service.GetAttachment(t.Context(), AttachmentRequest{MessageID: id, AttachmentID: attachmentID, Offset: offset, Length: 5})
		if err != nil {
			t.Fatalf("chunk at %d: %v", offset, err)
		}
		data, _ := base64.StdEncoding.DecodeString(chunk.DataBase64)
		if chunk.Size != int64(len(whole)) || chunk.Offset != offset || chunk.Length != int64(len(data)) {
			t.Fatalf("chunk metadata = %+v", chunk)
		}
		got = append(got, data...)
		if chunk.NextOffset == 0 {
			break
		}
		offset = chunk.NextOffset
	}
	if string(got) != whole {
		t.Fatalf("reassembled %q, want %q", got, whole)
	}
	if _, err := service.GetAttachment(t.Context(), AttachmentRequest{MessageID: id, AttachmentID: attachmentID, Offset: 100}); err == nil {
		t.Fatal("offset past the end was accepted")
	}
	_, err := service.GetAttachment(t.Context(), AttachmentRequest{MessageID: id, AttachmentID: attachmentID, MaxBytes: 3})
	if err == nil || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("oversized download error should point to offset/length: %v", err)
	}
}

func TestSearchKeepsGmailOrder(t *testing.T) {
	service, fake := newService(t)
	var ids []string
	for i := range 12 {
		ids = append(ids, fake.AddMessage(strings.Replace(inboundFromStranger, "Hello bot", fmt.Sprintf("Hello bot %d", i), 1), "INBOX"))
	}
	result, err := service.Search(t.Context(), SearchInput{Query: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, message := range result.Messages {
		got = append(got, message.ID)
	}
	want := slices.Clone(ids)
	slices.Reverse(want) // the fake lists newest first, like Gmail
	if !slices.Equal(got, want) {
		t.Fatalf("search order = %v, want Gmail order %v", got, want)
	}
}

func TestForwardEmbedsOnlyTheOriginalBody(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage("From: zach@hackclub.com\nTo: zrl-bot@hackclub.com\nSubject: Receipt\nContent-Type: text/html; charset=utf-8\n\n<html><head><title>t</title></head><body><p>Total $5</p></body></html>\n", "INBOX")
	if _, err := service.Forward(t.Context(), ForwardInput{MessageID: id, To: []string{"zach@hackclub.com"}, Text: "fyi"}); err != nil {
		t.Fatal(err)
	}
	decoded, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(fake.Sent()[0]))))
	raw := string(decoded)
	if !strings.Contains(raw, "<p>Total $5</p>") || strings.Contains(raw, "<html>") || strings.Contains(raw, "<head>") {
		t.Fatalf("forward should embed only the original body:\n%s", raw)
	}
}

// Gmail keeps each sender's Date header offset, so a thread mixes -0400,
// +0000 and -0700 and its raw dates read out of order. Every date the server
// returns is the same instant in one zone.
func TestDatesAreInOneZone(t *testing.T) {
	service, fake := newService(t)
	first := fake.AddMessage(strings.Replace(inboundFromZach, "Thu, 08 Oct 2026 10:00:00 -0400", "Thu, 08 Oct 2026 14:00:00 +0000", 1), "INBOX")
	original, _ := fake.Message(first)
	fake.AddMessageInThread(original.ThreadID, strings.Replace(zachFollowUp, "Thu, 08 Oct 2026 12:00:00 -0400", "Thu, 08 Oct 2026 09:00:00 -0700", 1), "INBOX")

	thread, err := service.GetThread(t.Context(), original.ThreadID, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{thread.Messages[0].Date, thread.Messages[1].Date}; !slices.Equal(got, []string{"Thu, 08 Oct 2026 10:00:00 -0400", "Thu, 08 Oct 2026 12:00:00 -0400"}) {
		t.Errorf("thread dates = %v", got)
	}
	search, err := service.Search(t.Context(), SearchInput{})
	if err != nil || search.Messages[1].Date != "Thu, 08 Oct 2026 10:00:00 -0400" {
		t.Errorf("search date = %#v, %v", search.Messages, err)
	}
	threads, err := service.SearchThreads(t.Context(), SearchInput{})
	if err != nil || threads.Threads[0].LastDate != "Thu, 08 Oct 2026 12:00:00 -0400" {
		t.Errorf("thread last_date = %#v, %v", threads.Threads, err)
	}
}

// Agents check who a reply went to; the send result says so directly.
func TestSendResultsListRecipients(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX")

	sent, err := service.Send(t.Context(), Outgoing{To: []string{"zach@hackclub.com"}, Bcc: []string{"zach@zachlatta.com"}, Subject: "Report", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sent.To, []string{"zach@hackclub.com"}) || sent.Cc != nil || !slices.Equal(sent.Bcc, []string{"zach@zachlatta.com"}) || sent.Subject != "Report" {
		t.Errorf("send result = %#v", sent)
	}
	reply, err := service.Reply(t.Context(), ReplyInput{MessageID: id, ReplyAll: true, Text: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reply.To, []string{"zach@hackclub.com"}) || !slices.Equal(reply.Cc, []string{"zach@zachlatta.com"}) || reply.Subject != "Re: Quarterly numbers" {
		t.Errorf("reply result = %#v", reply)
	}
	forward, err := service.Forward(t.Context(), ForwardInput{MessageID: id, To: []string{"zach@zachlatta.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(forward.To, []string{"zach@zachlatta.com"}) || forward.Subject != "Fwd: Quarterly numbers" {
		t.Errorf("forward result = %#v", forward)
	}
	draft, err := service.CreateDraft(t.Context(), Outgoing{To: []string{"zach@hackclub.com"}, Subject: "Draft", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	fromDraft, err := service.SendDraft(t.Context(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fromDraft.To, []string{"zach@hackclub.com"}) || fromDraft.Subject != "Draft" {
		t.Errorf("send_draft result = %#v", fromDraft)
	}
}

func TestMessageDetailLeavesOutThreadingHeaders(t *testing.T) {
	service, fake := newService(t)
	id := fake.AddMessage(inboundFromZach, "INBOX")
	original, _ := fake.Message(id)
	fake.AddMessageInThread(original.ThreadID, zachFollowUp, "INBOX")

	thread, err := service.GetThread(t.Context(), original.ThreadID, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(thread)
	for _, key := range []string{`"references"`, `"message_id_header"`, `"in_reply_to"`} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("get_thread output has %s, which grows with every reply:\n%s", key, encoded)
		}
	}
	withHeaders, err := service.GetMessage(t.Context(), id, GetOptions{IncludeHeaders: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(withHeaders.Headers, func(h Header) bool {
		return strings.EqualFold(h.Name, "Message-ID") && h.Value == "<orig-1@mail.gmail.com>"
	}) {
		t.Errorf("include_headers lost Message-ID: %#v", withHeaders.Headers)
	}
}

func TestGetThreadLatest(t *testing.T) {
	service, fake := newService(t)
	first := fake.AddMessage(inboundFromZach, "INBOX")
	original, _ := fake.Message(first)
	followUp := fake.AddMessageInThread(original.ThreadID, zachFollowUp, "INBOX")

	thread, err := service.GetThread(t.Context(), original.ThreadID, ThreadOptions{Latest: 1})
	if err != nil {
		t.Fatal(err)
	}
	if thread.MessageCount != 2 || len(thread.Messages) != 1 || thread.Messages[0].ID != followUp {
		t.Fatalf("latest 1 = count %d, %d messages, want only %s", thread.MessageCount, len(thread.Messages), followUp)
	}
	if got := strings.TrimSpace(thread.Messages[0].Text); got != "Any update?" {
		t.Errorf("latest message not trimmed: %q", got)
	}
	all, err := service.GetThread(t.Context(), original.ThreadID, ThreadOptions{Latest: 5})
	if err != nil || all.MessageCount != 2 || len(all.Messages) != 2 {
		t.Fatalf("latest beyond length = %#v, %v", all, err)
	}
	if got := strings.TrimSpace(all.Messages[0].Text); got != "Here are the numbers." {
		t.Errorf("first message changed: %q", got)
	}
}

func TestReplyArchivesThread(t *testing.T) {
	service, fake := newService(t)
	first := fake.AddMessage(inboundFromZach, "INBOX")
	original, _ := fake.Message(first)
	latest := fake.AddMessageInThread(original.ThreadID, zachFollowUp, "INBOX", "UNREAD")

	result, err := service.Reply(t.Context(), ReplyInput{MessageID: latest, Text: "Done.", ArchiveThread: true})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if !result.Archived || result.ArchiveError != "" || len(fake.Sent()) != 1 {
		t.Fatalf("result = %#v, sent %d", result, len(fake.Sent()))
	}
	for _, id := range []string{first, latest} {
		if message, _ := fake.Message(id); slices.Contains(message.LabelIDs, "INBOX") {
			t.Errorf("%s still in inbox: %v", id, message.LabelIDs)
		}
	}
}

func TestBlockedReplyDoesNotArchive(t *testing.T) {
	for name, input := range map[string]ReplyInput{
		"sender": {},
		"to":     {To: []string{"someone@example.com"}},
		"cc":     {To: []string{"zach@hackclub.com"}, Cc: []string{"someone@example.com"}},
		"bcc":    {To: []string{"zach@hackclub.com"}, Bcc: []string{"someone@example.com"}},
	} {
		t.Run(name, func(t *testing.T) {
			service, fake := newService(t)
			id := fake.AddMessage(inboundFromStranger, "INBOX")
			input.MessageID, input.Text, input.ArchiveThread = id, "sure", true
			_, err := service.Reply(t.Context(), input)
			assertBlocked(t, fake, err)
			if message, _ := fake.Message(id); !slices.Contains(message.LabelIDs, "INBOX") {
				t.Errorf("blocked reply archived the thread: %v", message.LabelIDs)
			}
		})
	}
}
