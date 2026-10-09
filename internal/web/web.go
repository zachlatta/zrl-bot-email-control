// Package web serves the public landing page and the secret-protected admin
// page used to connect the Gmail account and send a test email.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zachlatta/zrl-bot-email-control/internal/googleauth"
)

const (
	sessionCookie = "zrl_bot_admin"
	sessionTTL    = 12 * time.Hour
)

// Connector is the Google connection flow.
type Connector interface {
	Start() (string, error)
	Finish(context.Context, string) (string, error)
	Status(context.Context) googleauth.Status
	Disconnect(context.Context) error
	UsesLoopbackRedirect() bool
}

// TestSender sends a fixed test message to an allowlisted address.
type TestSender interface {
	SendTest(ctx context.Context, to string) error
}

type Config struct {
	Secret            string
	PublicURL         string
	Address           string
	AllowedRecipients []string
	Connector         Connector
	Sender            TestSender
	Version           string
}

type handler struct {
	cfg      Config
	secret   [32]byte
	mux      *http.ServeMux
	mu       sync.Mutex
	sessions map[string]time.Time
	secure   bool
}

func New(cfg Config) http.Handler {
	h := &handler{cfg: cfg, secret: sha256.Sum256([]byte(cfg.Secret)), sessions: map[string]time.Time{}, secure: strings.HasPrefix(cfg.PublicURL, "https://")}
	h.mux = http.NewServeMux()
	h.mux.HandleFunc("GET /{$}", h.landing)
	h.mux.HandleFunc("GET /admin", h.admin)
	h.mux.HandleFunc("POST /admin/login", h.login)
	h.mux.HandleFunc("POST /admin/logout", h.logout)
	h.mux.HandleFunc("POST /admin/google/start", h.requireSession(h.googleStart))
	h.mux.HandleFunc("POST /admin/google/finish", h.requireSession(h.googleFinish))
	// The Google redirect is a cross-site navigation, so the Strict session
	// cookie is absent; the single-use state from an admin-started flow is the
	// authorization here.
	h.mux.HandleFunc("GET "+googleauth.CallbackPath, h.googleCallback)
	h.mux.HandleFunc("POST /admin/google/disconnect", h.requireSession(h.googleDisconnect))
	h.mux.HandleFunc("POST /admin/test-send", h.requireSession(h.testSend))
	protection := http.NewCrossOriginProtection()
	_ = protection.AddTrustedOrigin(strings.TrimRight(cfg.PublicURL, "/"))
	return securityHeaders(protection.Handler(h.mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (h *handler) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	expires, ok := h.sessions[cookie.Value]
	if !ok || time.Now().After(expires) {
		delete(h.sessions, cookie.Value)
		return false
	}
	return true
}

func (h *handler) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.validSession(r) {
			http.Error(w, "unauthorized: log in at /admin", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (h *handler) checkSecret(value string) bool {
	provided := sha256.Sum256([]byte(value))
	return h.cfg.Secret != "" && subtle.ConstantTimeCompare(h.secret[:], provided[:]) == 1
}

func (h *handler) landing(w http.ResponseWriter, r *http.Request) {
	status := h.cfg.Connector.Status(r.Context())
	render(w, http.StatusOK, landingPage, map[string]any{
		"MCPURL":    strings.TrimRight(h.cfg.PublicURL, "/") + "/mcp",
		"Connected": status.Connected,
		"Version":   h.cfg.Version,
	})
}

func (h *handler) admin(w http.ResponseWriter, r *http.Request) {
	if !h.validSession(r) {
		render(w, http.StatusOK, loginPage, map[string]any{})
		return
	}
	h.dashboard(w, r, http.StatusOK, r.URL.Query().Get("notice"), r.URL.Query().Get("error"))
}

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request, code int, notice, problem string) {
	render(w, code, dashboardPage, map[string]any{
		"Address":  h.cfg.Address,
		"Allowed":  h.cfg.AllowedRecipients,
		"Status":   h.cfg.Connector.Status(r.Context()),
		"MCPURL":   strings.TrimRight(h.cfg.PublicURL, "/") + "/mcp",
		"Notice":   notice,
		"Error":    problem,
		"Version":  h.cfg.Version,
		"Loopback": h.cfg.Connector.UsesLoopbackRedirect(),
	})
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !h.checkSecret(r.PostForm.Get("secret")) {
		time.Sleep(500 * time.Millisecond)
		render(w, http.StatusUnauthorized, loginPage, map[string]any{"Error": "That secret is not valid."})
		return
	}
	session := rand.Text() + rand.Text()
	h.mu.Lock()
	for key, expires := range h.sessions {
		if time.Now().After(expires) {
			delete(h.sessions, key)
		}
	}
	h.sessions[session] = time.Now().Add(sessionTTL)
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: session, Path: "/admin", HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		h.mu.Lock()
		delete(h.sessions, cookie.Value)
		h.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *handler) googleStart(w http.ResponseWriter, r *http.Request) {
	authURL, err := h.cfg.Connector.Start()
	if err != nil {
		h.dashboard(w, r, http.StatusBadRequest, "", err.Error())
		return
	}
	if !h.cfg.Connector.UsesLoopbackRedirect() {
		http.Redirect(w, r, authURL, http.StatusSeeOther)
		return
	}
	render(w, http.StatusOK, connectPage, map[string]any{"AuthURL": template.URL(authURL), "Address": h.cfg.Address})
}

func (h *handler) googleFinish(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	h.finish(w, r, r.PostForm.Get("redirected_url"))
}

func (h *handler) googleCallback(w http.ResponseWriter, r *http.Request) {
	h.finish(w, r, "?"+r.URL.RawQuery)
}

func (h *handler) finish(w http.ResponseWriter, r *http.Request, value string) {
	email, err := h.cfg.Connector.Finish(r.Context(), value)
	if err != nil {
		redirectWith(w, r, "error", err.Error())
		return
	}
	redirectWith(w, r, "notice", "Connected "+email+".")
}

func (h *handler) googleDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := h.cfg.Connector.Disconnect(r.Context()); err != nil {
		redirectWith(w, r, "error", err.Error())
		return
	}
	redirectWith(w, r, "notice", "Disconnected. Agents cannot use the mailbox until it is reconnected.")
}

func (h *handler) testSend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	to := r.PostForm.Get("to")
	if !slices.Contains(h.cfg.AllowedRecipients, strings.ToLower(to)) {
		http.Error(w, "not an allowlisted recipient", http.StatusBadRequest)
		return
	}
	if err := h.cfg.Sender.SendTest(r.Context(), to); err != nil {
		redirectWith(w, r, "error", "Test email failed: "+err.Error())
		return
	}
	redirectWith(w, r, "notice", "Sent a test email to "+to+".")
}

func redirectWith(w http.ResponseWriter, r *http.Request, key, value string) {
	http.Redirect(w, r, "/admin?"+url.Values{key: {value}}.Encode(), http.StatusSeeOther)
}

func render(w http.ResponseWriter, code int, page *template.Template, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = page.Execute(w, data)
}

const style = `<style>body{font-family:system-ui,sans-serif;max-width:40rem;margin:6vh auto;padding:1.5rem;color:#17202a;line-height:1.5}
form{display:grid;gap:.75rem;margin:1rem 0}input,button,select{font:inherit;padding:.6rem}button{cursor:pointer;background:#17202a;color:#fff;border:0;border-radius:.35rem}
.secondary{background:#ccd1d9;color:#17202a}.error{color:#a00}.notice{color:#060}code{background:#f2f4f7;padding:.1rem .3rem;border-radius:.25rem}.ok{color:#060}.bad{color:#a00}</style>`

var landingPage = template.Must(template.New("landing").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>zrl-bot email</title>` + style + `</head>
<body><h1>zrl-bot email</h1><p>An MCP server that gives authorized agents control of one mailbox. Outgoing mail is restricted to a fixed allowlist.</p>
<p>MCP endpoint: <code>{{.MCPURL}}</code></p><p>Mailbox: {{if .Connected}}<span class="ok">connected</span>{{else}}<span class="bad">not connected</span>{{end}}</p>
<p><a href="/admin">Admin</a> · <small>{{.Version}}</small></p></body></html>`))

var loginPage = template.Must(template.New("login").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Admin · zrl-bot email</title>` + style + `</head>
<body><h1>Admin</h1>{{with .Error}}<p class="error">{{.}}</p>{{end}}
<form method="post" action="/admin/login"><label>Secret <input type="password" name="secret" required autofocus autocomplete="current-password"></label><button type="submit">Log in</button></form></body></html>`))

var dashboardPage = template.Must(template.New("dashboard").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Admin · zrl-bot email</title>` + style + `</head>
<body><h1>zrl-bot email admin</h1>
{{with .Notice}}<p class="notice">{{.}}</p>{{end}}{{with .Error}}<p class="error">{{.}}</p>{{end}}
<h2>Mailbox</h2><p><strong>{{.Address}}</strong>: {{if .Status.Connected}}<span class="ok">connected</span>{{else}}<span class="bad">not connected</span>{{with .Status.Error}} — {{.}}{{end}}{{end}}</p>
{{if .Status.Configured}}<form method="post" action="/admin/google/start"><button type="submit">{{if .Status.Connected}}Reconnect Gmail{{else}}Connect Gmail{{end}}</button></form>{{else}}<p class="error">GOOGLE_OAUTH_CLIENT_JSON is not configured.</p>{{end}}
{{if .Status.Connected}}<form method="post" action="/admin/google/disconnect"><button class="secondary" type="submit">Disconnect</button></form>{{end}}
<h2>Outbound allowlist</h2>{{if .Allowed}}<ul>{{range .Allowed}}<li>{{.}}</li>{{end}}</ul>{{else}}<p class="bad">Empty: sending is disabled.</p>{{end}}
{{if and .Status.Connected .Allowed}}<form method="post" action="/admin/test-send"><label>Send a test email to <select name="to">{{range .Allowed}}<option>{{.}}</option>{{end}}</select></label><button type="submit">Send test email</button></form>{{end}}
<h2>Agents</h2><p>Add <code>{{.MCPURL}}</code> as a remote MCP server. The client registers itself with OAuth; approve it with the same secret you used here.</p>
<form method="post" action="/admin/logout"><button class="secondary" type="submit">Log out</button></form><p><small>{{.Version}}</small></p></body></html>`))

var connectPage = template.Must(template.New("connect").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect Gmail · zrl-bot email</title>` + style + `</head>
<body><h1>Connect Gmail</h1><ol>
<li><a href="{{.AuthURL}}" target="_blank" rel="noopener noreferrer">Open Google sign-in</a> and sign in as <strong>{{.Address}}</strong> (use a private window if you are signed in to another account). Approve every permission.</li>
<li>Google then sends your browser to a <code>http://localhost/?state=…&amp;code=…</code> page that will fail to load. That is expected.</li>
<li>Copy the whole URL from that tab's address bar and paste it here:</li></ol>
<form method="post" action="/admin/google/finish"><input name="redirected_url" placeholder="http://localhost/?state=...&code=..." required autocomplete="off"><button type="submit">Finish connecting</button></form>
<p><a href="/admin">Cancel</a></p></body></html>`))
