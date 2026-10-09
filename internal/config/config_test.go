package config

import (
	"strings"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

const secret = "0123456789abcdef0123456789abcdef"

func TestDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"MCP_SECRET": secret}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MailboxAddress != "zrl-bot@hackclub.com" || cfg.Addr != "127.0.0.1:8080" || cfg.PublicURL != "http://127.0.0.1:8080" || cfg.DataDir != "data" {
		t.Fatalf("defaults = %#v", cfg)
	}
	if len(cfg.Allowlist.Addresses()) != 0 {
		t.Fatalf("default allowlist should be empty: %v", cfg.Allowlist.Addresses())
	}
}

func TestFullConfig(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"MCP_SECRET":               secret,
		"MAILBOX_ADDRESS":          "Bot@Example.com",
		"ALLOWED_RECIPIENTS":       "zach@hackclub.com, zach@zachlatta.com",
		"PUBLIC_URL":               "https://mail.example.com/",
		"ADDR":                     "0.0.0.0:8080",
		"DATA_DIR":                 "/data",
		"GOOGLE_OAUTH_CLIENT_JSON": `{"installed":{}}`,
		"SOURCE_COMMIT":            "deadbeef",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MailboxAddress != "bot@example.com" || cfg.PublicURL != "https://mail.example.com" || cfg.GoogleClientJSON == "" || cfg.Version != "deadbeef" {
		t.Fatalf("cfg = %#v", cfg)
	}
	if strings.Join(cfg.Allowlist.Addresses(), ",") != "zach@hackclub.com,zach@zachlatta.com" {
		t.Fatalf("allowlist = %v", cfg.Allowlist.Addresses())
	}
}

func TestRejectsUnsafeConfig(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"missing secret":   {},
		"short secret":     {"MCP_SECRET": "short"},
		"bad allowlist":    {"MCP_SECRET": secret, "ALLOWED_RECIPIENTS": "Zach <zach@hackclub.com>"},
		"bad mailbox":      {"MCP_SECRET": secret, "MAILBOX_ADDRESS": "nope"},
		"public http":      {"MCP_SECRET": secret, "PUBLIC_URL": "http://mail.example.com"},
		"public with path": {"MCP_SECRET": secret, "PUBLIC_URL": "https://mail.example.com/sub"},
	} {
		if _, err := Load(env(values)); err == nil {
			t.Errorf("%s: Load succeeded", name)
		}
	}
}
