package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zachlatta/zrl-bot-email-control/internal/googleauth"
)

type fakeConnector struct {
	started   int
	finished  []string
	status    googleauth.Status
	loopback  bool
	finishErr error
}

func (f *fakeConnector) Start() (string, error) {
	f.started++
	return "https://accounts.google.com/o/oauth2/auth?state=s", nil
}
func (f *fakeConnector) Finish(_ context.Context, value string) (string, error) {
	f.finished = append(f.finished, value)
	return "zrl-bot@hackclub.com", f.finishErr
}
func (f *fakeConnector) Status(context.Context) googleauth.Status { return f.status }
func (f *fakeConnector) Disconnect(context.Context) error         { return nil }
func (f *fakeConnector) UsesLoopbackRedirect() bool               { return f.loopback }

type fakeSender struct{ sent []string }

func (f *fakeSender) SendTest(_ context.Context, to string) error {
	f.sent = append(f.sent, to)
	return nil
}

func newHandler(t *testing.T) (http.Handler, *fakeConnector, *fakeSender) {
	connector := &fakeConnector{loopback: true, status: googleauth.Status{Configured: true}}
	sender := &fakeSender{}
	handler := New(Config{
		Secret:            "correct-horse-battery-staple-123",
		PublicURL:         "https://mail.example.com",
		Address:           "zrl-bot@hackclub.com",
		AllowedRecipients: []string{"zach@hackclub.com", "zach@zachlatta.com"},
		Connector:         connector,
		Sender:            sender,
		Version:           "abc123",
	})
	return handler, connector, sender
}

func do(handler http.Handler, method, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	request := httptest.NewRequest(method, "https://mail.example.com"+target, body)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "https://mail.example.com")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func login(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	response := do(handler, http.MethodPost, "/admin/login", url.Values{"secret": {"correct-horse-battery-staple-123"}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d: %s", response.Code, response.Body)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
				t.Errorf("weak session cookie: %#v", cookie)
			}
			return cookie
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestLandingPageIsPublicAndHarmless(t *testing.T) {
	handler, _, _ := newHandler(t)
	response := do(handler, http.MethodGet, "/", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "https://mail.example.com/mcp") {
		t.Fatalf("landing = %d %s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), "zach@zachlatta.com") {
		t.Error("landing page leaks the allowlist")
	}
}

func TestAdminRequiresSecret(t *testing.T) {
	handler, connector, sender := newHandler(t)
	if response := do(handler, http.MethodGet, "/admin", nil); !strings.Contains(response.Body.String(), `name="secret"`) {
		t.Fatalf("anonymous /admin should show login: %s", response.Body)
	}
	if response := do(handler, http.MethodPost, "/admin/login", url.Values{"secret": {"wrong"}}); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret status = %d", response.Code)
	}
	for _, path := range []string{"/admin/google/start", "/admin/google/finish", "/admin/google/disconnect", "/admin/test-send"} {
		response := do(handler, http.MethodPost, path, url.Values{"redirected_url": {"x"}, "to": {"zach@hackclub.com"}})
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s without session = %d", path, response.Code)
		}
	}
	forged := &http.Cookie{Name: sessionCookie, Value: "forged"}
	if response := do(handler, http.MethodPost, "/admin/google/start", url.Values{}, forged); response.Code != http.StatusUnauthorized {
		t.Errorf("forged session = %d", response.Code)
	}
	if connector.started != 0 || len(sender.sent) != 0 {
		t.Fatal("unauthenticated request had side effects")
	}
}

func TestConnectFlow(t *testing.T) {
	handler, connector, _ := newHandler(t)
	session := login(t, handler)
	dashboard := do(handler, http.MethodGet, "/admin", nil, session)
	if !strings.Contains(dashboard.Body.String(), "zach@zachlatta.com") || !strings.Contains(dashboard.Body.String(), "Connect Gmail") {
		t.Fatalf("dashboard = %s", dashboard.Body)
	}
	start := do(handler, http.MethodPost, "/admin/google/start", url.Values{}, session)
	if start.Code != http.StatusOK || !strings.Contains(start.Body.String(), "accounts.google.com") || !strings.Contains(start.Body.String(), `name="redirected_url"`) {
		t.Fatalf("start = %d %s", start.Code, start.Body)
	}
	finish := do(handler, http.MethodPost, "/admin/google/finish", url.Values{"redirected_url": {"http://localhost/?state=s&code=c"}}, session)
	if finish.Code != http.StatusSeeOther || len(connector.finished) != 1 {
		t.Fatalf("finish = %d %v", finish.Code, connector.finished)
	}
}

func TestTestSendOnlyToAllowlist(t *testing.T) {
	handler, _, sender := newHandler(t)
	session := login(t, handler)
	if response := do(handler, http.MethodPost, "/admin/test-send", url.Values{"to": {"someone@example.com"}}, session); response.Code != http.StatusBadRequest {
		t.Fatalf("test-send to stranger = %d", response.Code)
	}
	if response := do(handler, http.MethodPost, "/admin/test-send", url.Values{"to": {"zach@hackclub.com"}}, session); response.Code != http.StatusSeeOther {
		t.Fatalf("test-send = %d %s", response.Code, response.Body)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "zach@hackclub.com" {
		t.Fatalf("sent = %v", sender.sent)
	}
}

func TestCrossSiteAdminPostRejected(t *testing.T) {
	handler, connector, _ := newHandler(t)
	session := login(t, handler)
	request := httptest.NewRequest(http.MethodPost, "https://mail.example.com/admin/google/start", strings.NewReader(""))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://evil.example.com")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.AddCookie(session)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || connector.started != 0 {
		t.Fatalf("cross-site post = %d (started %d)", recorder.Code, connector.started)
	}
}
