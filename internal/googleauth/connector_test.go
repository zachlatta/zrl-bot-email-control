package googleauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zachlatta/zrl-bot-email-control/internal/gmailfake"
	"github.com/zachlatta/zrl-bot-email-control/internal/mailbox"
	"github.com/zachlatta/zrl-bot-email-control/internal/store"
	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

const bot = "zrl-bot@hackclub.com"

type tokenServer struct {
	mu        sync.Mutex
	verifiers map[string]string // code -> expected challenge
	refreshes int
	server    *httptest.Server
	revoked   bool
}

func newTokenServer(t *testing.T) *tokenServer {
	ts := &tokenServer{verifiers: map[string]string{}}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			challenge, ok := ts.verifiers[r.PostForm.Get("code")]
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.PostForm.Get("redirect_uri") == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "Bearer", "expires_in": 3600})
		case "refresh_token":
			if ts.revoked {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`))
				return
			}
			ts.refreshes++
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-refreshed", "token_type": "Bearer", "expires_in": 3600})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func setup(t *testing.T, mailboxAddress string) (*Connector, *tokenServer, *store.FileStore) {
	t.Helper()
	tokens := newTokenServer(t)
	fake := gmailfake.New(t, mailboxAddress)
	fileStore, err := store.Open(filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	connector := New(Config{
		OAuth: &oauth2.Config{
			ClientID: "client-id", ClientSecret: "client-secret",
			Endpoint:    oauth2.Endpoint{AuthURL: "https://accounts.example.com/o/oauth2/auth", TokenURL: tokens.server.URL},
			RedirectURL: "http://localhost",
			Scopes:      Scopes,
		},
		Address: bot,
		Store:   fileStore,
		NewGmail: func(ctx context.Context, source oauth2.TokenSource) (*gmail.Service, error) {
			return gmail.NewService(ctx, option.WithEndpoint(fake.URL()+"/"), option.WithHTTPClient(oauth2.NewClient(ctx, source)))
		},
	})
	return connector, tokens, fileStore
}

func approve(t *testing.T, tokens *tokenServer, authURL string) string {
	t.Helper()
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	tokens.mu.Lock()
	tokens.verifiers["the-code"] = query.Get("code_challenge")
	tokens.mu.Unlock()
	return "http://localhost/?state=" + url.QueryEscape(query.Get("state")) + "&code=the-code&scope=" + url.QueryEscape(strings.Join(Scopes, " "))
}

func TestStartBuildsOfflinePKCEURL(t *testing.T) {
	connector, _, _ := setup(t, bot)
	authURL, err := connector.Start()
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(authURL)
	query := parsed.Query()
	for key, want := range map[string]string{
		"client_id": "client-id", "redirect_uri": "http://localhost", "access_type": "offline",
		"prompt": "consent", "login_hint": bot, "code_challenge_method": "S256", "response_type": "code",
	} {
		if query.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, query.Get(key), want)
		}
	}
	if query.Get("state") == "" || len(query.Get("code_challenge")) < 43 || !strings.Contains(query.Get("scope"), "https://mail.google.com/") {
		t.Errorf("auth URL missing state/challenge/scope: %s", authURL)
	}
}

func TestFinishStoresTokenForCorrectMailbox(t *testing.T) {
	connector, tokens, fileStore := setup(t, bot)
	if _, err := connector.Gmail(t.Context()); !errors.Is(err, mailbox.ErrNotConnected) {
		t.Fatalf("Gmail before connect = %v", err)
	}
	authURL, _ := connector.Start()
	email, err := connector.Finish(t.Context(), approve(t, tokens, authURL))
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if email != bot {
		t.Fatalf("email = %q", email)
	}
	token, ok, _ := fileStore.GoogleToken(t.Context())
	if !ok || token.RefreshToken != "refresh-1" {
		t.Fatalf("stored token = %#v", token)
	}
	status := connector.Status(t.Context())
	if !status.Connected || status.Email != bot {
		t.Fatalf("status = %#v", status)
	}
	// The pending state is single-use.
	if _, err := connector.Finish(t.Context(), approve(t, tokens, authURL)); err == nil {
		t.Fatal("state reused")
	}
}

func TestFinishRejectsWrongAccount(t *testing.T) {
	connector, tokens, fileStore := setup(t, "zach@hackclub.com")
	authURL, _ := connector.Start()
	if _, err := connector.Finish(t.Context(), approve(t, tokens, authURL)); err == nil || !strings.Contains(err.Error(), "zach@hackclub.com") {
		t.Fatalf("Finish with wrong account = %v", err)
	}
	if _, ok, _ := fileStore.GoogleToken(t.Context()); ok {
		t.Fatal("token for the wrong account was stored")
	}
}

func TestFinishRejectsUnknownState(t *testing.T) {
	connector, tokens, _ := setup(t, bot)
	authURL, _ := connector.Start()
	pasted := approve(t, tokens, authURL)
	tampered := strings.Replace(pasted, "state=", "state=x", 1)
	if _, err := connector.Finish(t.Context(), tampered); err == nil {
		t.Fatal("unknown state accepted")
	}
	if _, err := connector.Finish(t.Context(), "http://localhost/?error=access_denied&state=x"); err == nil {
		t.Fatal("error redirect accepted")
	}
}

func TestRefreshPersistsAndKeepsRefreshToken(t *testing.T) {
	connector, tokens, fileStore := setup(t, bot)
	if err := fileStore.SaveGoogleToken(t.Context(), &oauth2.Token{AccessToken: "stale", RefreshToken: "refresh-1", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	service, err := connector.Gmail(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Users.GetProfile("me").Do(); err != nil {
		t.Fatal(err)
	}
	token, _, _ := fileStore.GoogleToken(t.Context())
	if token.AccessToken != "access-refreshed" || token.RefreshToken != "refresh-1" || tokens.refreshes != 1 {
		t.Fatalf("after refresh: %#v (refreshes %d)", token, tokens.refreshes)
	}
}

func TestRevokedTokenReportsReconnect(t *testing.T) {
	connector, tokens, fileStore := setup(t, bot)
	tokens.revoked = true
	_ = fileStore.SaveGoogleToken(t.Context(), &oauth2.Token{AccessToken: "stale", RefreshToken: "refresh-1", Expiry: time.Now().Add(-time.Hour)})
	status := connector.Status(t.Context())
	if status.Connected || !strings.Contains(status.Error, "reconnect") {
		t.Fatalf("status = %#v", status)
	}
}

func TestParseClientJSON(t *testing.T) {
	installed := `{"installed":{"client_id":"id","client_secret":"sec","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token","redirect_uris":["http://localhost"]}}`
	config, err := ParseClientJSON([]byte(installed), "https://mail.example.com")
	if err != nil || config.RedirectURL != "http://localhost" || config.ClientID != "id" {
		t.Fatalf("installed = %#v, %v", config, err)
	}
	web := `{"web":{"client_id":"id","client_secret":"sec","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token","redirect_uris":["https://mail.example.com/admin/google/callback"]}}`
	config, err = ParseClientJSON([]byte(web), "https://mail.example.com")
	if err != nil || config.RedirectURL != "https://mail.example.com/admin/google/callback" {
		t.Fatalf("web = %#v, %v", config, err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(installed))
	if _, err := ParseClientJSON([]byte(encoded), "https://mail.example.com"); err != nil {
		t.Fatalf("base64 client JSON: %v", err)
	}
	if _, err := ParseClientJSON([]byte(`{"web":{"client_id":"id","redirect_uris":["https://other.example.com/cb"]}}`), "https://mail.example.com"); err == nil {
		t.Fatal("web client without this server's callback accepted")
	}
}
