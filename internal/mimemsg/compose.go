// Package mimemsg composes RFC 5322 / MIME messages from structured input.
// Agents never supply raw headers: every header value is validated here, and
// the composed bytes are still checked by recipients.Allowlist before sending.
package mimemsg

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
)

// Attachment is a file to include in a message.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
	// ContentID makes the attachment an inline part that the HTML body shows
	// with <img src="cid:ContentID">. Empty means a regular attachment.
	ContentID string
}

// Message is structured input for Compose. Addresses must be bare addr-specs.
type Message struct {
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	ReplyTo     []string
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
	InReplyTo   string
	References  string
	Now         func() time.Time
}

// Compose renders a message as multipart/mixed (text and/or HTML alternative,
// then attachments) with CRLF line endings. Attachments with a ContentID go
// with the body in a multipart/related part, so mail clients show them inline.
func Compose(message Message) ([]byte, error) {
	if len(message.To)+len(message.Cc)+len(message.Bcc) == 0 {
		return nil, errors.New("message needs at least one recipient")
	}
	now := time.Now
	if message.Now != nil {
		now = message.Now
	}
	header := textproto.MIMEHeader{}
	var order []string
	set := func(key, value string) {
		if _, exists := header[key]; !exists {
			order = append(order, key)
		}
		header.Set(key, value)
	}
	if message.From != "" {
		from, err := addressList([]string{message.From})
		if err != nil {
			return nil, fmt.Errorf("from: %w", err)
		}
		set("From", from)
	}
	for _, field := range []struct {
		name  string
		value []string
	}{{"To", message.To}, {"Cc", message.Cc}, {"Bcc", message.Bcc}, {"Reply-To", message.ReplyTo}} {
		if len(field.value) == 0 {
			continue
		}
		value, err := addressList(field.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", strings.ToLower(field.name), err)
		}
		set(field.name, value)
	}
	if err := checkHeaderText("subject", message.Subject); err != nil {
		return nil, err
	}
	set("Subject", mime.QEncoding.Encode("utf-8", message.Subject))
	set("Date", now().Format(time.RFC1123Z))
	set("Message-ID", fmt.Sprintf("<%s@zrl-bot-email-control>", strings.ToLower(rand.Text())))
	for name, value := range map[string]string{"In-Reply-To": message.InReplyTo, "References": message.References} {
		if value == "" {
			continue
		}
		if err := checkHeaderText(strings.ToLower(name), value); err != nil {
			return nil, err
		}
		if !isASCII(value) {
			return nil, fmt.Errorf("%s must be ASCII message IDs", strings.ToLower(name))
		}
		set(name, value)
	}
	set("MIME-Version", "1.0")

	var body bytes.Buffer
	mixed := multipart.NewWriter(&body)
	set("Content-Type", mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": mixed.Boundary()}))

	var inline, regular []Attachment
	for _, attachment := range message.Attachments {
		if attachment.ContentID == "" {
			regular = append(regular, attachment)
			continue
		}
		if err := checkContentID(attachment.ContentID); err != nil {
			return nil, fmt.Errorf("attachment %q: %w", attachment.Filename, err)
		}
		inline = append(inline, attachment)
	}
	if len(inline) == 0 {
		if err := writeAlternative(mixed, message); err != nil {
			return nil, err
		}
	} else if err := writeRelated(mixed, message, inline); err != nil {
		return nil, err
	}
	for _, attachment := range regular {
		if err := writeAttachment(mixed, attachment); err != nil {
			return nil, err
		}
	}
	if err := mixed.Close(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	for _, key := range order {
		fmt.Fprintf(&out, "%s: %s\r\n", key, header.Get(key))
	}
	out.WriteString("\r\n")
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// writeAlternative writes a multipart/alternative part holding the plain
// text body and, when present, the HTML body.
func writeAlternative(mixed *multipart.Writer, message Message) error {
	var buffer bytes.Buffer
	inner := multipart.NewWriter(&buffer)
	bodies := []struct{ mediaType, content string }{{"text/plain", message.Text}}
	if message.HTML != "" {
		bodies = append(bodies, struct{ mediaType, content string }{"text/html", message.HTML})
	}
	for _, body := range bodies {
		header := textproto.MIMEHeader{}
		header.Set("Content-Type", body.mediaType+"; charset=utf-8")
		header.Set("Content-Transfer-Encoding", "quoted-printable")
		part, err := inner.CreatePart(header)
		if err != nil {
			return err
		}
		if err := writeQuotedPrintable(part, body.content); err != nil {
			return err
		}
	}
	if err := inner.Close(); err != nil {
		return err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": inner.Boundary()}))
	part, err := mixed.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(buffer.Bytes())
	return err
}

// writeRelated writes a multipart/related part holding the body alternative
// and the inline parts it references by Content-ID.
func writeRelated(mixed *multipart.Writer, message Message, inline []Attachment) error {
	var buffer bytes.Buffer
	related := multipart.NewWriter(&buffer)
	if err := writeAlternative(related, message); err != nil {
		return err
	}
	for _, attachment := range inline {
		if err := writeAttachment(related, attachment); err != nil {
			return err
		}
	}
	if err := related.Close(); err != nil {
		return err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType("multipart/related", map[string]string{"boundary": related.Boundary(), "type": "multipart/alternative"}))
	part, err := mixed.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(buffer.Bytes())
	return err
}

// checkContentID accepts the dot-atom style IDs agents pass (letters, digits
// and . _ - + @), so the value cannot break out of the Content-ID header.
func checkContentID(id string) error {
	if len(id) > 200 {
		return errors.New("content_id is longer than 200 characters")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._-+@", c) >= 0) {
			return fmt.Errorf("content_id %q may only contain letters, digits and . _ - + @", id)
		}
	}
	return nil
}

func writeAttachment(writer *multipart.Writer, attachment Attachment) error {
	filename := attachment.Filename
	if filename == "" {
		filename = "attachment"
	}
	if err := checkHeaderText("attachment filename", filename); err != nil {
		return err
	}
	contentType := attachment.ContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("attachment %q content type: %w", filename, err)
	}
	params["name"] = filename
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	disposition := "attachment"
	if attachment.ContentID != "" {
		disposition = "inline"
		header.Set("Content-ID", "<"+attachment.ContentID+">")
	}
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	header.Set("Content-Transfer-Encoding", "base64")
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	return writeBase64Lines(part, attachment.Data)
}

func writeBase64Lines(w interface{ Write([]byte) (int, error) }, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	for len(encoded) > 76 {
		if _, err := w.Write([]byte(encoded[:76] + "\r\n")); err != nil {
			return err
		}
		encoded = encoded[76:]
	}
	_, err := w.Write([]byte(encoded + "\r\n"))
	return err
}

func addressList(addresses []string) (string, error) {
	formatted := make([]string, 0, len(addresses))
	for _, address := range addresses {
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, address) {
			return "", fmt.Errorf("%q is not a bare email address", address)
		}
		if !isASCII(parsed.Address) {
			return "", fmt.Errorf("%q must be ASCII", address)
		}
		formatted = append(formatted, parsed.Address)
	}
	return strings.Join(formatted, ", "), nil
}

func checkHeaderText(field, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s must not contain line breaks or NUL", field)
	}
	return nil
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func writeQuotedPrintable(w interface{ Write([]byte) (int, error) }, text string) error {
	encoder := quotedprintable.NewWriter(w)
	normalized := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	if _, err := encoder.Write([]byte(normalized)); err != nil {
		return err
	}
	return encoder.Close()
}
