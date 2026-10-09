package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zachlatta/zrl-bot-email-control/internal/config"
	"github.com/zachlatta/zrl-bot-email-control/internal/gmailfake"
	"github.com/zachlatta/zrl-bot-email-control/internal/store"
	"google.golang.org/api/gmail/v1"
)

const secret = "0123456789abcdef0123456789abcdef"

func startServer(t *testing.T) (*httptest.Server, *gmailfake.Server) {
	t.Helper()
	fake := gmailfake.New(t, "zrl-bot@hackclub.com")
	server := httptest.NewUnstartedServer(nil)
	publicURL := "http://" + server.Listener.Addr().String()
	cfg, err := config.Load(func(key string) string {
		return map[string]string{
			"MCP_SECRET":         secret,
			"ALLOWED_RECIPIENTS": "zach@hackclub.com,zach@zachlatta.com",
			"PUBLIC_URL":         publicURL,
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	fileStore, err := store.Open(filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(cfg, fileStore, Options{Gmail: func(context.Context) (*gmail.Service, error) { return fake.Service(t), nil }})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return server, fake
}

func TestHealthAndUnauthenticatedMCP(t *testing.T) {
	server, _ := startServer(t)
	response, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	_ = json.NewDecoder(response.Body).Decode(&health)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || health["status"] != "ok" || health["mailbox_connected"] != true {
		t.Fatalf("health = %d %v", response.StatusCode, health)
	}
	response, err = http.Post(server.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatalf("anonymous /mcp = %d %q", response.StatusCode, response.Header.Get("WWW-Authenticate"))
	}
}

func TestOAuthThenMCPEndToEnd(t *testing.T) {
	server, fake := startServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	registration, err := client.Post(server.URL+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"e2e","redirect_uris":["http://127.0.0.1:9/callback"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var registered struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(registration.Body).Decode(&registered)
	registration.Body.Close()

	verifier := strings.Repeat("v", 64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	form := url.Values{
		"client_id": {registered.ClientID}, "redirect_uri": {"http://127.0.0.1:9/callback"}, "response_type": {"code"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"xyz"}, "secret": {secret},
	}
	authorize, err := client.PostForm(server.URL+"/oauth/authorize", form)
	if err != nil {
		t.Fatal(err)
	}
	authorize.Body.Close()
	location, _ := url.Parse(authorize.Header.Get("Location"))
	code := location.Query().Get("code")
	if authorize.StatusCode != http.StatusFound || code == "" {
		t.Fatalf("authorize = %d %s", authorize.StatusCode, authorize.Header.Get("Location"))
	}
	tokenResponse, err := client.PostForm(server.URL+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {registered.ClientID},
		"redirect_uri": {"http://127.0.0.1:9/callback"}, "code_verifier": {verifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(tokenResponse.Body).Decode(&token)
	tokenResponse.Body.Close()
	if token.AccessToken == "" {
		t.Fatal("no access token")
	}

	transport := &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token.AccessToken}}}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "test"}, nil).Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connect MCP: %v", err)
	}
	defer session.Close()

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_email", Arguments: map[string]any{"to": []string{"evil@example.com"}, "text": "hi"}})
	if err != nil || !result.IsError {
		t.Fatalf("send to stranger over HTTP: err=%v result=%#v", err, result)
	}
	result, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_email", Arguments: map[string]any{"to": []string{"zach@hackclub.com"}, "subject": "e2e", "text": "hi"}})
	if err != nil || result.IsError {
		t.Fatalf("send to zach over HTTP: err=%v result=%#v", err, result)
	}
	if len(fake.Sent()) != 1 {
		t.Fatalf("sent = %d", len(fake.Sent()))
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
