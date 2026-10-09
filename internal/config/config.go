// Package config reads server configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"strings"

	"github.com/zachlatta/zrl-bot-email-control/internal/recipients"
)

const minSecretLength = 24

type Config struct {
	MailboxAddress   string
	Allowlist        *recipients.Allowlist
	Secret           string
	PublicURL        string
	Addr             string
	DataDir          string
	GoogleClientJSON string
	Version          string
}

// Load reads configuration through getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	get := func(key, fallback string) string {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			return value
		}
		return fallback
	}
	cfg := Config{
		MailboxAddress:   strings.ToLower(get("MAILBOX_ADDRESS", "zrl-bot@hackclub.com")),
		Secret:           getenv("MCP_SECRET"),
		Addr:             get("ADDR", "127.0.0.1:8080"),
		DataDir:          get("DATA_DIR", "data"),
		GoogleClientJSON: get("GOOGLE_OAUTH_CLIENT_JSON", ""),
		Version:          get("SOURCE_COMMIT", get("GIT_SHA", "dev")),
	}
	cfg.PublicURL = strings.TrimRight(get("PUBLIC_URL", "http://"+cfg.Addr), "/")

	if len(cfg.Secret) < minSecretLength {
		return Config{}, fmt.Errorf("MCP_SECRET must be at least %d characters", minSecretLength)
	}
	if parsed, err := mail.ParseAddress(cfg.MailboxAddress); err != nil || parsed.Name != "" || parsed.Address != cfg.MailboxAddress {
		return Config{}, fmt.Errorf("MAILBOX_ADDRESS %q is not a bare email address", cfg.MailboxAddress)
	}
	allowlist, err := recipients.ParseAllowlist(getenv("ALLOWED_RECIPIENTS"))
	if err != nil {
		return Config{}, fmt.Errorf("ALLOWED_RECIPIENTS: %w", err)
	}
	cfg.Allowlist = allowlist
	if err := checkPublicURL(cfg.PublicURL); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func checkPublicURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("PUBLIC_URL %q is not an absolute URL", value)
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("PUBLIC_URL must be an origin with no path, e.g. https://mail.example.com")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if parsed.Scheme == "http" && (host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		return nil
	}
	return errors.New("PUBLIC_URL must use https unless it is a loopback address")
}
