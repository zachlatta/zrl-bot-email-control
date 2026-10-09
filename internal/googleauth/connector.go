// Package googleauth connects the server to the bot's Gmail account with a
// one-time Google OAuth consent and keeps the token refreshed and persisted.
package googleauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zachlatta/zrl-bot-email-control/internal/mailbox"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Scopes grant full mailbox control: mail.google.com covers read, modify,
// send and permanent delete; settings.basic is required for filters.
var Scopes = []string{"https://mail.google.com/", gmail.GmailSettingsBasicScope}

// CallbackPath receives the Google redirect when a web OAuth client is used.
const CallbackPath = "/admin/google/callback"

// TokenStore persists the mailbox token.
type TokenStore interface {
	GoogleToken(context.Context) (*oauth2.Token, bool, error)
	SaveGoogleToken(context.Context, *oauth2.Token) error
	ClearGoogleToken(context.Context) error
}

type Config struct {
	OAuth    *oauth2.Config
	Address  string
	Store    TokenStore
	NewGmail func(context.Context, oauth2.TokenSource) (*gmail.Service, error)
	Now      func() time.Time
}

type pending struct {
	verifier string
	expires  time.Time
}

// Connector implements the consent flow and supplies authorized clients.
type Connector struct {
	cfg     Config
	mu      sync.Mutex
	pending map[string]pending
}

func New(cfg Config) *Connector {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewGmail == nil {
		cfg.NewGmail = func(ctx context.Context, source oauth2.TokenSource) (*gmail.Service, error) {
			return gmail.NewService(ctx, option.WithTokenSource(source))
		}
	}
	return &Connector{cfg: cfg, pending: map[string]pending{}}
}

// Configured reports whether a Google OAuth client is available.
func (c *Connector) Configured() bool { return c != nil && c.cfg.OAuth != nil }

// UsesLoopbackRedirect reports whether the admin must paste the redirect URL
// back (desktop OAuth clients) rather than being redirected automatically.
func (c *Connector) UsesLoopbackRedirect() bool {
	return c.Configured() && !strings.HasSuffix(c.cfg.OAuth.RedirectURL, CallbackPath)
}

// Start begins a consent flow and returns the Google authorization URL.
func (c *Connector) Start() (string, error) {
	if !c.Configured() {
		return "", errors.New("no Google OAuth client is configured (set GOOGLE_OAUTH_CLIENT_JSON)")
	}
	state := strings.ToLower(oauth2.GenerateVerifier()[:32])
	verifier := oauth2.GenerateVerifier()
	c.mu.Lock()
	now := c.cfg.Now()
	for key, value := range c.pending {
		if now.After(value.expires) {
			delete(c.pending, key)
		}
	}
	c.pending[state] = pending{verifier: verifier, expires: now.Add(30 * time.Minute)}
	c.mu.Unlock()
	return c.cfg.OAuth.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("login_hint", c.cfg.Address),
		oauth2.SetAuthURLParam("include_granted_scopes", "true"),
	), nil
}

// Finish completes the flow from the URL Google redirected to (or its query
// string). It verifies the account is the configured mailbox before storing.
func (c *Connector) Finish(ctx context.Context, redirected string) (string, error) {
	query, err := parseRedirect(redirected)
	if err != nil {
		return "", err
	}
	if problem := query.Get("error"); problem != "" {
		return "", fmt.Errorf("google returned %s", problem)
	}
	state, code := query.Get("state"), query.Get("code")
	if state == "" || code == "" {
		return "", errors.New("paste the full URL from the browser address bar after approving; it must contain code= and state=")
	}
	c.mu.Lock()
	flow, ok := c.pending[state]
	delete(c.pending, state)
	c.mu.Unlock()
	if !ok || c.cfg.Now().After(flow.expires) {
		return "", errors.New("this authorization link has expired or was already used; start again")
	}
	token, err := c.cfg.OAuth.Exchange(ctx, code, oauth2.VerifierOption(flow.verifier))
	if err != nil {
		return "", fmt.Errorf("exchange authorization code: %w", err)
	}
	if token.RefreshToken == "" {
		return "", errors.New("google did not return a refresh token; remove the app's access at myaccount.google.com/permissions and try again")
	}
	service, err := c.cfg.NewGmail(ctx, c.cfg.OAuth.TokenSource(ctx, token))
	if err != nil {
		return "", err
	}
	profile, err := service.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("read Gmail profile: %w", err)
	}
	if !strings.EqualFold(profile.EmailAddress, c.cfg.Address) {
		return "", fmt.Errorf("you signed in as %s, but this server controls %s; sign in as %s instead", profile.EmailAddress, c.cfg.Address, c.cfg.Address)
	}
	if err := c.cfg.Store.SaveGoogleToken(ctx, token); err != nil {
		return "", fmt.Errorf("save token: %w", err)
	}
	return profile.EmailAddress, nil
}

func parseRedirect(value string) (url.Values, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("nothing was pasted")
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("parse pasted URL: %w", err)
		}
		return parsed.Query(), nil
	}
	return url.ParseQuery(strings.TrimPrefix(value, "?"))
}

// Gmail returns an authorized client; refreshed tokens are persisted.
func (c *Connector) Gmail(ctx context.Context) (*gmail.Service, error) {
	if !c.Configured() {
		return nil, mailbox.ErrNotConnected
	}
	token, ok, err := c.cfg.Store.GoogleToken(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, mailbox.ErrNotConnected
	}
	// The token source outlives this request's context only for refreshes,
	// which must not be cancelled half-way by a closing request.
	base := c.cfg.OAuth.TokenSource(context.WithoutCancel(ctx), token)
	source := &persistingSource{base: oauth2.ReuseTokenSource(token, base), store: c.cfg.Store, last: token.AccessToken}
	return c.cfg.NewGmail(ctx, source)
}

type persistingSource struct {
	base  oauth2.TokenSource
	store TokenStore
	mu    sync.Mutex
	last  string
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	token, err := p.base.Token()
	if err != nil {
		var retrieve *oauth2.RetrieveError
		if errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant" {
			return nil, fmt.Errorf("google authorization for the mailbox was revoked or expired; an admin must reconnect it at /admin: %w", err)
		}
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if token.AccessToken != p.last {
		p.last = token.AccessToken
		_ = p.store.SaveGoogleToken(context.Background(), token)
	}
	return token, nil
}

// Status describes the mailbox connection.
type Status struct {
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	Email      string `json:"email,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Status checks the connection live against Gmail.
func (c *Connector) Status(ctx context.Context) Status {
	status := Status{Configured: c.Configured()}
	service, err := c.Gmail(ctx)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	profile, err := service.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		status.Error = err.Error()
		if !strings.Contains(status.Error, "reconnect") {
			status.Error += " (reconnect at /admin if this persists)"
		}
		return status
	}
	status.Email = profile.EmailAddress
	status.Connected = strings.EqualFold(profile.EmailAddress, c.cfg.Address)
	if !status.Connected {
		status.Error = fmt.Sprintf("connected account %s is not %s", profile.EmailAddress, c.cfg.Address)
	}
	return status
}

// Disconnect forgets the stored token.
func (c *Connector) Disconnect(ctx context.Context) error {
	return c.cfg.Store.ClearGoogleToken(ctx)
}

// ParseClientJSON reads a Google OAuth client file (raw JSON or base64).
// Desktop ("installed") clients use the http://localhost loopback redirect and
// a paste-back step; web clients must list publicURL + CallbackPath.
func ParseClientJSON(data []byte, publicURL string) (*oauth2.Config, error) {
	trimmed := strings.TrimSpace(string(data))
	if !strings.HasPrefix(trimmed, "{") {
		decoded, err := base64.StdEncoding.DecodeString(trimmed)
		if err != nil {
			return nil, errors.New("google OAuth client must be JSON or base64-encoded JSON")
		}
		trimmed = string(decoded)
	}
	var file struct {
		Installed *struct {
			RedirectURIs []string `json:"redirect_uris"`
		} `json:"installed"`
		Web *struct {
			RedirectURIs []string `json:"redirect_uris"`
		} `json:"web"`
	}
	if err := json.Unmarshal([]byte(trimmed), &file); err != nil {
		return nil, fmt.Errorf("decode google OAuth client: %w", err)
	}
	var redirect string
	switch {
	case file.Web != nil:
		want := strings.TrimRight(publicURL, "/") + CallbackPath
		for _, uri := range file.Web.RedirectURIs {
			if uri == want {
				redirect = want
			}
		}
		if redirect == "" {
			return nil, fmt.Errorf("web OAuth client must list %s as an authorized redirect URI", want)
		}
	case file.Installed != nil:
		redirect = "http://localhost"
	default:
		return nil, errors.New("google OAuth client JSON must contain an \"installed\" or \"web\" client")
	}
	config, err := google.ConfigFromJSON([]byte(trimmed), Scopes...)
	if err != nil {
		return nil, fmt.Errorf("google OAuth client: %w", err)
	}
	config.RedirectURL = redirect
	return config, nil
}
