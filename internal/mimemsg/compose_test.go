package mimemsg

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"

	"github.com/zachlatta/zrl-bot-email-control/internal/recipients"
)

func parse(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse composed message: %v\n%s", err, raw)
	}
	return msg
}

func TestComposePlainText(t *testing.T) {
	raw, err := Compose(Message{
		From:    "zrl-bot@hackclub.com",
		To:      []string{"zach@hackclub.com"},
		Cc:      []string{"zach@zachlatta.com"},
		Bcc:     []string{"zach+b@hackclub.com"},
		Subject: "Héllo — world",
		Text:    "line one\nline two ✓",
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	msg := parse(t, raw)
	if got := msg.Header.Get("To"); got != "<zach@hackclub.com>" && got != "zach@hackclub.com" {
		t.Errorf("To = %q", got)
	}
	if msg.Header.Get("Bcc") == "" || msg.Header.Get("Cc") == "" {
		t.Errorf("missing Cc/Bcc headers: %v", msg.Header)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subject != "Héllo — world" {
		t.Errorf("Subject = %q", subject)
	}
	if msg.Header.Get("MIME-Version") != "1.0" || msg.Header.Get("Date") == "" || msg.Header.Get("Message-Id") == "" {
		t.Errorf("missing standard headers: %v", msg.Header)
	}
	addresses, err := recipients.RecipientsOfRaw(raw)
	if err != nil || strings.Join(addresses, ",") != "zach@hackclub.com,zach@zachlatta.com,zach+b@hackclub.com" {
		t.Errorf("RecipientsOfRaw = %v, %v", addresses, err)
	}
	mediaType, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mediaType != "multipart/mixed" {
		t.Fatalf("Content-Type = %s", mediaType)
	}
	text := findPart(t, multipart.NewReader(msg.Body, params["boundary"]), "text/plain")
	if text != "line one\nline two ✓" {
		t.Errorf("text body = %q", text)
	}
}

func TestComposeHTMLAndAttachments(t *testing.T) {
	raw, err := Compose(Message{
		From:    "zrl-bot@hackclub.com",
		To:      []string{"zach@hackclub.com"},
		Subject: "files",
		Text:    "see attached",
		HTML:    "<p>see attached</p>",
		Attachments: []Attachment{
			{Filename: "report ü.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 fake")},
			{Filename: "notes.txt", Data: []byte("hello")},
		},
		InReplyTo:  "<orig@mail.gmail.com>",
		References: "<root@mail.gmail.com> <orig@mail.gmail.com>",
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	msg := parse(t, raw)
	if msg.Header.Get("In-Reply-To") != "<orig@mail.gmail.com>" || !strings.Contains(msg.Header.Get("References"), "<root@mail.gmail.com>") {
		t.Errorf("threading headers = %v", msg.Header)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	reader := multipart.NewReader(msg.Body, params["boundary"])
	var sawAlternative bool
	attachments := map[string]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		mediaType, partParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if mediaType == "multipart/alternative" {
			sawAlternative = true
			inner := multipart.NewReader(part, partParams["boundary"])
			if html := findPart(t, inner, "text/html"); html != "<p>see attached</p>" {
				t.Errorf("html = %q", html)
			}
			continue
		}
		data, _ := io.ReadAll(part) // multipart.Reader decodes quoted-printable only, so decode base64 by hand
		decoded, err := decodeBase64(data)
		if err != nil {
			t.Fatalf("decode attachment: %v", err)
		}
		attachments[part.FileName()] = mediaType + ":" + string(decoded)
	}
	if !sawAlternative {
		t.Error("missing multipart/alternative body")
	}
	if attachments["report ü.pdf"] != "application/pdf:%PDF-1.4 fake" {
		t.Errorf("pdf attachment = %q (all %v)", attachments["report ü.pdf"], attachments)
	}
	if !strings.HasPrefix(attachments["notes.txt"], "text/plain") || !strings.HasSuffix(attachments["notes.txt"], ":hello") {
		t.Errorf("txt attachment = %q", attachments["notes.txt"])
	}
}

func TestComposeInlineImages(t *testing.T) {
	raw, err := Compose(Message{
		From:    "zrl-bot@hackclub.com",
		To:      []string{"zach@hackclub.com"},
		Subject: "candles",
		Text:    "photos below",
		HTML:    `<p>photos below</p><img src="cid:candle1">`,
		Attachments: []Attachment{
			{Filename: "candle.jpg", ContentType: "image/jpeg", Data: []byte("jpegbytes"), ContentID: "candle1"},
			{Filename: "notes.txt", Data: []byte("hello")},
		},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	msg := parse(t, raw)
	mediaType, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mediaType != "multipart/mixed" {
		t.Fatalf("top type = %q", mediaType)
	}
	reader := multipart.NewReader(msg.Body, params["boundary"])
	var sawRelated, sawInline, sawHTML bool
	var regular []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		partType, partParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if partType != "multipart/related" {
			regular = append(regular, part.FileName())
			continue
		}
		sawRelated = true
		inner := multipart.NewReader(part, partParams["boundary"])
		for {
			sub, err := inner.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("related NextPart: %v", err)
			}
			subType, subParams, _ := mime.ParseMediaType(sub.Header.Get("Content-Type"))
			switch {
			case subType == "multipart/alternative":
				if html := findPart(t, multipart.NewReader(sub, subParams["boundary"]), "text/html"); strings.Contains(html, "cid:candle1") {
					sawHTML = true
				}
			case subType == "image/jpeg":
				if sub.Header.Get("Content-ID") != "<candle1>" {
					t.Errorf("Content-ID = %q", sub.Header.Get("Content-ID"))
				}
				if !strings.HasPrefix(sub.Header.Get("Content-Disposition"), "inline") {
					t.Errorf("Content-Disposition = %q", sub.Header.Get("Content-Disposition"))
				}
				data, _ := io.ReadAll(sub)
				decoded, err := decodeBase64(data)
				if err != nil || string(decoded) != "jpegbytes" {
					t.Errorf("inline image data = %q (%v)", decoded, err)
				}
				sawInline = true
			}
		}
	}
	if !sawRelated || !sawInline || !sawHTML {
		t.Errorf("related=%v inline=%v html=%v", sawRelated, sawInline, sawHTML)
	}
	if len(regular) != 1 || regular[0] != "notes.txt" {
		t.Errorf("regular attachments = %v", regular)
	}
}

func TestComposeRejectsBadContentID(t *testing.T) {
	for _, id := range []string{"a b", "<x>", "x\r\nBcc: y@z.com", "ü"} {
		_, err := Compose(Message{
			To:          []string{"zach@hackclub.com"},
			HTML:        "<img src=\"cid:x\">",
			Attachments: []Attachment{{Filename: "a.png", Data: []byte("x"), ContentID: id}},
		})
		if err == nil {
			t.Errorf("content id %q accepted", id)
		}
	}
}

func TestComposeRejectsHeaderInjection(t *testing.T) {
	cases := map[string]Message{
		"subject crlf":   {From: "a@b.com", To: []string{"zach@hackclub.com"}, Subject: "hi\r\nBcc: evil@example.com"},
		"to with name":   {From: "a@b.com", To: []string{"Evil <evil@example.com>"}},
		"to crlf":        {From: "a@b.com", To: []string{"zach@hackclub.com\r\nBcc: evil@example.com"}},
		"in-reply-to lf": {From: "a@b.com", To: []string{"zach@hackclub.com"}, InReplyTo: "<x>\nBcc: evil@example.com"},
		"filename crlf":  {From: "a@b.com", To: []string{"zach@hackclub.com"}, Attachments: []Attachment{{Filename: "a\r\nBcc: e@x.com", Data: []byte("x")}}},
		"no recipients":  {From: "a@b.com"},
	}
	for name, message := range cases {
		if _, err := Compose(message); err == nil {
			t.Errorf("%s: Compose succeeded, want error", name)
		}
	}
}

func findPart(t *testing.T, reader *multipart.Reader, want string) string {
	t.Helper()
	for {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatalf("part %s not found: %v", want, err)
		}
		mediaType, params, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if strings.HasPrefix(mediaType, "multipart/") && want != mediaType {
			return findPart(t, multipart.NewReader(part, params["boundary"]), want)
		}
		if mediaType == want {
			data, _ := io.ReadAll(part)
			return strings.ReplaceAll(string(data), "\r\n", "\n")
		}
	}
}

func decodeBase64(data []byte) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(data)), ""))
}
