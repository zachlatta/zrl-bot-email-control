package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zachlatta/zrl-bot-email-control/internal/auth"
	"golang.org/x/oauth2"
)

var _ auth.Store = (*FileStore)(nil)

func TestFileStorePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	first, err := Open(path, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.SaveClient(ctx, auth.Client{ID: "c1", Name: "Claude", RedirectURIs: []string{"https://claude.ai/cb"}}); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveRefreshToken(ctx, "rhash", auth.Token{ClientID: "c1", Resource: "r", Scope: "email", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveToken(ctx, "ahash", auth.Token{ClientID: "c1", ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := first.SaveGoogleToken(ctx, &oauth2.Token{RefreshToken: "google-refresh", AccessToken: "a", Expiry: now}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600", info.Mode().Perm())
	}

	second, err := Open(path, func() time.Time { return now })
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	client, ok, err := second.Client(ctx, "c1")
	if err != nil || !ok || client.Name != "Claude" || client.RedirectURIs[0] != "https://claude.ai/cb" {
		t.Fatalf("Client after reopen = %#v, %v, %v", client, ok, err)
	}
	refresh, ok, _ := second.RefreshToken(ctx, "rhash")
	if !ok || refresh.Scope != "email" {
		t.Fatalf("RefreshToken after reopen = %#v, %v", refresh, ok)
	}
	if _, ok, _ := second.Token(ctx, "ahash"); !ok {
		t.Fatal("access token lost after reopen")
	}
	google, ok, err := second.GoogleToken(ctx)
	if err != nil || !ok || google.RefreshToken != "google-refresh" {
		t.Fatalf("GoogleToken after reopen = %#v, %v, %v", google, ok, err)
	}
}

func TestTakeCodeIsSingleUse(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "state.json"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCode(ctx, "h", auth.Code{ClientID: "c", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.TakeCode(ctx, "h"); !ok {
		t.Fatal("first TakeCode missed")
	}
	if _, ok, _ := store.TakeCode(ctx, "h"); ok {
		t.Fatal("second TakeCode succeeded")
	}
}

func TestExpiredEntriesArePrunedOnWrite(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store, err := Open(filepath.Join(t.TempDir(), "state.json"), clock)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SaveToken(ctx, "old", auth.Token{ExpiresAt: now.Add(time.Minute)})
	now = now.Add(2 * time.Minute)
	_ = store.SaveToken(ctx, "new", auth.Token{ExpiresAt: now.Add(time.Minute)})
	if _, ok, _ := store.Token(ctx, "old"); ok {
		t.Fatal("expired token survived a write")
	}
	if _, ok, _ := store.Token(ctx, "new"); !ok {
		t.Fatal("fresh token missing")
	}
}

func TestOpenRejectsCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, time.Now); err == nil {
		t.Fatal("Open accepted corrupt state")
	}
}

func TestClearGoogleToken(t *testing.T) {
	ctx := context.Background()
	store, _ := Open(filepath.Join(t.TempDir(), "state.json"), time.Now)
	_ = store.SaveGoogleToken(ctx, &oauth2.Token{RefreshToken: "x"})
	if err := store.ClearGoogleToken(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.GoogleToken(ctx); ok {
		t.Fatal("token still present after clear")
	}
}

func TestClientRegistrationsAreCapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range auth.MaxClients + 5 {
		client := auth.Client{ID: fmt.Sprintf("client-%03d", i), RedirectURIs: []string{"https://x.example/cb"}, CreatedAt: start.Add(time.Duration(i) * time.Minute)}
		if err := s.SaveClient(ctx, client); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range map[int]bool{0: false, 4: false, 5: true, auth.MaxClients + 4: true} {
		if _, ok, _ := reopened.Client(ctx, fmt.Sprintf("client-%03d", i)); ok != want {
			t.Errorf("client-%03d present = %v, want %v", i, ok, want)
		}
	}
}
