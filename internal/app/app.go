// Package app wires configuration, storage, OAuth, the mailbox service, the
// MCP endpoint and the admin UI into one HTTP handler.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/zrl-bot-email-control/internal/auth"
	"github.com/zachlatta/zrl-bot-email-control/internal/config"
	"github.com/zachlatta/zrl-bot-email-control/internal/googleauth"
	"github.com/zachlatta/zrl-bot-email-control/internal/mailbox"
	"github.com/zachlatta/zrl-bot-email-control/internal/mcpserver"
	"github.com/zachlatta/zrl-bot-email-control/internal/store"
	"github.com/zachlatta/zrl-bot-email-control/internal/web"
	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
)

// Options overrides dependencies in tests.
type Options struct {
	// Gmail replaces the Google connector as the source of Gmail clients.
	Gmail func(context.Context) (*gmail.Service, error)
}

// New builds the complete HTTP handler.
func New(cfg config.Config, fileStore *store.FileStore, options Options) (http.Handler, error) {
	var oauthConfig *oauth2.Config
	if cfg.GoogleClientJSON != "" {
		parsed, err := googleauth.ParseClientJSON([]byte(cfg.GoogleClientJSON), cfg.PublicURL)
		if err != nil {
			return nil, err
		}
		oauthConfig = parsed
	}
	connector := googleauth.New(googleauth.Config{OAuth: oauthConfig, Address: cfg.MailboxAddress, Store: fileStore})
	gmailSource := connector.Gmail
	var status func(context.Context) googleauth.Status = connector.Status
	if options.Gmail != nil {
		gmailSource = options.Gmail
		status = func(ctx context.Context) googleauth.Status {
			_, err := options.Gmail(ctx)
			return googleauth.Status{Configured: true, Connected: err == nil, Email: cfg.MailboxAddress}
		}
	}
	service := mailbox.New(mailbox.Config{Address: cfg.MailboxAddress, Allowlist: cfg.Allowlist, Gmail: gmailSource})

	oauthServer := auth.NewServer(auth.Config{Issuer: cfg.PublicURL, Secret: cfg.Secret, Store: fileStore})
	webHandler := web.New(web.Config{
		Secret: cfg.Secret, PublicURL: cfg.PublicURL, Address: cfg.MailboxAddress,
		AllowedRecipients: cfg.Allowlist.Addresses(),
		Connector:         statusOverride{Connector: connector, status: status},
		Sender:            testSender{service: service, version: cfg.Version},
		Version:           cfg.Version,
	})

	mux := http.NewServeMux()
	oauthServer.RegisterRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		current := status(ctx)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "version": cfg.Version, "mailbox": cfg.MailboxAddress,
			"mailbox_connected": current.Connected, "allowed_recipient_count": len(cfg.Allowlist.Addresses()),
		})
	})
	mcpServer := mcpserver.New(service, cfg.Version)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	crossOrigin := http.NewCrossOriginProtection()
	if err := crossOrigin.AddTrustedOrigin(cfg.PublicURL); err != nil {
		return nil, fmt.Errorf("configure MCP origin protection: %w", err)
	}
	mux.Handle("/mcp", crossOrigin.Handler(oauthServer.RequireBearer(mcpHandler)))
	mux.Handle("/", webHandler)
	return mux, nil
}

type statusOverride struct {
	*googleauth.Connector
	status func(context.Context) googleauth.Status
}

func (s statusOverride) Status(ctx context.Context) googleauth.Status { return s.status(ctx) }

type testSender struct {
	service *mailbox.Service
	version string
}

func (t testSender) SendTest(ctx context.Context, to string) error {
	if to == "" {
		return errors.New("recipient is required")
	}
	_, err := t.service.Send(ctx, mailbox.Outgoing{
		To:      []string{to},
		Subject: "zrl-bot email control: test message",
		Text:    fmt.Sprintf("This is a test message from %s, sent from the admin page at %s (version %s).", t.service.Address(), time.Now().UTC().Format(time.RFC3339), t.version),
	})
	return err
}
