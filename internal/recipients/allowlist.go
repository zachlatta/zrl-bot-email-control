// Package recipients is the hard outbound gate. Every message the server
// sends, forwards, or stores as a sendable draft must pass Allowlist.CheckRaw
// on its final RFC 5322 bytes immediately before it reaches Gmail. Nothing in
// this package can be configured at runtime except the list of addresses,
// which is read once from the environment at startup.
package recipients

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"net/textproto"
	"slices"
	"strings"
)

// ErrRecipientNotAllowed is returned (wrapped) whenever a message or address
// list would reach anyone outside the allowlist, or cannot be proven not to.
var ErrRecipientNotAllowed = errors.New("recipient not allowed")

// recipientHeaders are the headers Gmail uses to decide delivery.
var recipientHeaders = []string{"To", "Cc", "Bcc"}

// Allowlist is an immutable set of exact, lowercase email addresses.
type Allowlist struct {
	addresses map[string]struct{}
}

// ParseAllowlist parses a comma-separated list of bare addresses
// ("a@example.com,b@example.com"). Display names, groups, and anything that is
// not a plain addr-spec are rejected so the configuration is unambiguous.
func ParseAllowlist(raw string) (*Allowlist, error) {
	list := &Allowlist{addresses: map[string]struct{}{}}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		parsed, err := mail.ParseAddress(part)
		if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, part) {
			return nil, fmt.Errorf("invalid allowlisted address %q: must be a bare email address", part)
		}
		list.addresses[strings.ToLower(parsed.Address)] = struct{}{}
	}
	return list, nil
}

// Addresses returns the allowlisted addresses, sorted.
func (l *Allowlist) Addresses() []string {
	out := make([]string, 0, len(l.addresses))
	for address := range l.addresses {
		out = append(out, address)
	}
	slices.Sort(out)
	return out
}

// Allowed reports whether address exactly matches an allowlisted address,
// ignoring case only. No trimming, plus-address folding, or domain matching.
func (l *Allowlist) Allowed(address string) bool {
	_, ok := l.addresses[strings.ToLower(address)]
	return ok
}

// CheckAddresses verifies that every address is allowlisted and that there is
// at least one.
func (l *Allowlist) CheckAddresses(addresses []string) error {
	if len(addresses) == 0 {
		return fmt.Errorf("%w: message has no recipients", ErrRecipientNotAllowed)
	}
	var blocked []string
	for _, address := range addresses {
		if !l.Allowed(address) {
			blocked = append(blocked, strings.ToLower(address))
		}
	}
	if len(blocked) > 0 {
		return fmt.Errorf("%w: %s (allowed: %s)", ErrRecipientNotAllowed, strings.Join(blocked, ", "), l.describe())
	}
	return nil
}

func (l *Allowlist) describe() string {
	if len(l.addresses) == 0 {
		return "none; sending is disabled"
	}
	return strings.Join(l.Addresses(), ", ")
}

// CheckRaw parses the header block of a complete RFC 5322 message and verifies
// every delivery recipient in every To, Cc, and Bcc header (including repeated
// headers) is allowlisted. Resent-* headers and anything unparseable are
// rejected outright.
func (l *Allowlist) CheckRaw(raw []byte) error {
	addresses, err := RecipientsOfRaw(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRecipientNotAllowed, err)
	}
	return l.CheckAddresses(addresses)
}

// RecipientsOfRaw extracts every delivery address from a raw message's To, Cc
// and Bcc headers. It errors on any ambiguity.
func RecipientsOfRaw(raw []byte) ([]string, error) {
	end := headerEnd(raw)
	if end < 0 {
		return nil, errors.New("message has no complete header block")
	}
	if err := checkHeaderBytes(raw[:end]); err != nil {
		return nil, err
	}
	reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw[:end])))
	header, err := reader.ReadMIMEHeader()
	if err != nil {
		return nil, fmt.Errorf("parse message headers: %w", err)
	}
	for key := range header {
		if strings.HasPrefix(strings.ToLower(key), "resent-") {
			return nil, fmt.Errorf("%s headers are not allowed", key)
		}
	}
	var addresses []string
	for _, name := range recipientHeaders {
		for _, value := range header.Values(name) {
			if strings.TrimSpace(value) == "" {
				continue
			}
			list, err := (&mail.AddressParser{}).ParseList(value)
			if err != nil {
				return nil, fmt.Errorf("parse %s header: %w", name, err)
			}
			for _, address := range list {
				addresses = append(addresses, address.Address)
			}
		}
	}
	return addresses, nil
}

// headerEnd returns the offset just past the blank line that ends the header
// block, or -1 when the message has none.
func headerEnd(raw []byte) int {
	crlf := bytes.Index(raw, []byte("\r\n\r\n"))
	lf := bytes.Index(raw, []byte("\n\n"))
	switch {
	case crlf < 0 && lf < 0:
		return -1
	case crlf < 0:
		return lf + 2
	case lf < 0 || crlf < lf:
		return crlf + 4
	default:
		return lf + 2
	}
}

// checkHeaderBytes requires a strictly 7-bit header block with no NUL and no
// CR outside CRLF, so no downstream parser can read different recipients than
// this one did.
func checkHeaderBytes(header []byte) error {
	for i, b := range header {
		switch {
		case b == '\r':
			if i+1 >= len(header) || header[i+1] != '\n' {
				return errors.New("message headers contain a bare carriage return")
			}
		case b == '\n' || b == '\t':
		case b < 0x20 || b > 0x7e:
			return fmt.Errorf("message headers contain non-ASCII or control byte 0x%02x", b)
		}
	}
	return nil
}
