package recipients

import (
	"errors"
	"strings"
	"testing"
)

func mustAllowlist(t *testing.T, raw string) *Allowlist {
	t.Helper()
	list, err := ParseAllowlist(raw)
	if err != nil {
		t.Fatalf("ParseAllowlist(%q): %v", raw, err)
	}
	return list
}

func TestParseAllowlistNormalizesAndRejectsGarbage(t *testing.T) {
	list := mustAllowlist(t, " Zach@HackClub.com, zach@zachlatta.com ,,")
	if got := list.Addresses(); strings.Join(got, ",") != "zach@hackclub.com,zach@zachlatta.com" {
		t.Fatalf("Addresses() = %v", got)
	}
	for _, bad := range []string{"not-an-email", "Zach <zach@hackclub.com>", "a@b.com;c@d.com", "@hackclub.com", "zach@"} {
		if _, err := ParseAllowlist(bad); err == nil {
			t.Errorf("ParseAllowlist(%q) succeeded, want error", bad)
		}
	}
}

func TestEmptyAllowlistBlocksEverything(t *testing.T) {
	list := mustAllowlist(t, "")
	err := list.CheckAddresses([]string{"zach@hackclub.com"})
	if !errors.Is(err, ErrRecipientNotAllowed) {
		t.Fatalf("CheckAddresses with empty allowlist = %v, want ErrRecipientNotAllowed", err)
	}
}

func TestAllowedIsExactCaseInsensitive(t *testing.T) {
	list := mustAllowlist(t, "zach@hackclub.com")
	cases := map[string]bool{
		"zach@hackclub.com":              true,
		"ZACH@HACKCLUB.COM":              true,
		"zach+tag@hackclub.com":          false,
		"zach@hackclub.com.evil.com":     false,
		"evilzach@hackclub.com":          false,
		"zach@hackclub.co":               false,
		" zach@hackclub.com":             false,
		"zach@hackclub.com\n":            false,
		"\"zach@hackclub.com\"@evil.com": false,
	}
	for address, want := range cases {
		if got := list.Allowed(address); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", address, got, want)
		}
	}
}

func TestCheckRawMessageAllowsOnlyAllowlistedRecipients(t *testing.T) {
	list := mustAllowlist(t, "zach@hackclub.com,zach@zachlatta.com")
	ok := "From: zrl-bot@hackclub.com\r\nTo: Zach <zach@hackclub.com>\r\nCc: zach@zachlatta.com\r\nSubject: hi\r\n\r\nbody"
	if err := list.CheckRaw([]byte(ok)); err != nil {
		t.Fatalf("CheckRaw(allowed) = %v", err)
	}

	blocked := map[string]string{
		"to":              "To: someone@example.com\r\n",
		"mixed to":        "To: zach@hackclub.com, someone@example.com\r\n",
		"cc":              "To: zach@hackclub.com\r\nCc: someone@example.com\r\n",
		"bcc":             "To: zach@hackclub.com\r\nBcc: someone@example.com\r\n",
		"lowercase bcc":   "To: zach@hackclub.com\r\nbcc: someone@example.com\r\n",
		"second to":       "To: zach@hackclub.com\r\nTo: someone@example.com\r\n",
		"second bcc":      "To: zach@hackclub.com\r\nBcc: zach@zachlatta.com\r\nBcc: someone@example.com\r\n",
		"resent-to":       "To: zach@hackclub.com\r\nResent-To: zach@zachlatta.com\r\n",
		"no recipients":   "",
		"empty to":        "To: \r\n",
		"group syntax":    "To: friends: someone@example.com;\r\n",
		"unparseable":     "To: <<<>>>\r\n",
		"encoded addr":    "To: =?utf-8?q?zach=40hackclub.com?= <someone@example.com>\r\n",
		"display spoof":   "To: \"zach@hackclub.com\" <someone@example.com>\r\n",
		"folded bcc":      "To: zach@hackclub.com\r\nBcc: zach@zachlatta.com,\r\n someone@example.com\r\n",
		"plus addressing": "To: zach+x@hackclub.com\r\n",
	}
	for name, headers := range blocked {
		raw := "From: zrl-bot@hackclub.com\r\n" + headers + "Subject: hi\r\n\r\nbody"
		err := list.CheckRaw([]byte(raw))
		if !errors.Is(err, ErrRecipientNotAllowed) {
			t.Errorf("%s: CheckRaw = %v, want ErrRecipientNotAllowed", name, err)
		}
	}
}

func TestCheckRawRejectsMalformedMessages(t *testing.T) {
	list := mustAllowlist(t, "zach@hackclub.com")
	for name, raw := range map[string]string{
		"no header terminator": "To: zach@hackclub.com",
		"empty":                "",
		"bare lf header":       "To zach@hackclub.com\n\nbody",
	} {
		if err := list.CheckRaw([]byte(raw)); !errors.Is(err, ErrRecipientNotAllowed) {
			t.Errorf("%s: CheckRaw = %v, want ErrRecipientNotAllowed", name, err)
		}
	}
}

func TestCheckRawAcceptsLFLineEndings(t *testing.T) {
	list := mustAllowlist(t, "zach@hackclub.com")
	if err := list.CheckRaw([]byte("To: zach@hackclub.com\nSubject: x\n\nbody")); err != nil {
		t.Fatalf("CheckRaw(LF) = %v", err)
	}
}

func TestBlockedErrorNamesOffendingAddresses(t *testing.T) {
	list := mustAllowlist(t, "zach@hackclub.com")
	err := list.CheckAddresses([]string{"zach@hackclub.com", "Other@Example.com"})
	if err == nil || !strings.Contains(err.Error(), "other@example.com") {
		t.Fatalf("error %v should name the blocked address", err)
	}
}

func TestCheckRawRejectsParserDifferentialBytes(t *testing.T) {
	list := mustAllowlist(t, "zach@hackclub.com")
	for name, raw := range map[string]string{
		"bare cr":  "To: zach@hackclub.com\rBcc: someone@example.com\r\nSubject: x\r\n\r\nbody",
		"nul":      "To: zach@hackclub.com\x00\r\nBcc: someone@example.com\r\n\r\nbody",
		"8bit hdr": "To: zach@hackclub.com\r\nSubject: caf\xe9\r\n\r\nbody",
	} {
		if err := list.CheckRaw([]byte(raw)); !errors.Is(err, ErrRecipientNotAllowed) {
			t.Errorf("%s: CheckRaw = %v, want ErrRecipientNotAllowed", name, err)
		}
	}
}
