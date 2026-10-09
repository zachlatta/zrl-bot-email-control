package mailbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatedCalls may appear only in internal/mailbox/gate.go, where each call is
// preceded by the allowlist check.
var gatedCalls = []string{".Messages.Send(", ".Drafts.Create(", ".Drafts.Update("}

// forbiddenCalls must never appear: each could deliver mail (or auto-replies,
// or forwarding verification emails) to arbitrary addresses, or bypass the
// gate by talking to Gmail directly.
var forbiddenCalls = []string{
	".Drafts.Send(",
	".Messages.Import(",
	".Messages.Insert(",
	"ForwardingAddresses",
	"UpdateAutoForwarding",
	"UpdateVacation",
	"SendAs",
	"Delegates",
	"UpdateImap",
	"UpdatePop",
	"gmail.googleapis.com",
	"smtp.",
	"net/smtp",
}

func TestOutboundCallsOnlyInGate(t *testing.T) {
	root := filepath.Join("..", "..")
	gate := filepath.Clean(filepath.Join("internal", "mailbox", "gate.go"))
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "vendor") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, _ := filepath.Rel(root, path)
		if strings.HasPrefix(relative, filepath.Join("internal", "gmailfake")) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(data)
		for _, call := range gatedCalls {
			if strings.Contains(source, call) {
				found[call] = true
				if relative != gate {
					t.Errorf("%s calls %s outside the outbound gate", relative, call)
				}
			}
		}
		for _, call := range forbiddenCalls {
			if strings.Contains(source, call) {
				t.Errorf("%s uses forbidden %s", relative, call)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range gatedCalls {
		if !found[call] {
			t.Errorf("expected gate.go to contain %s; contract scan may be broken", call)
		}
	}
}

func TestGateChecksBeforeEveryOutboundCall(t *testing.T) {
	data, err := os.ReadFile("gate.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, call := range gatedCalls {
		index := strings.Index(source, call)
		check := strings.LastIndex(source[:index], "s.cfg.Allowlist.CheckRaw(raw)")
		function := strings.LastIndex(source[:index], "\nfunc ")
		if check < 0 || check < function {
			t.Errorf("%s in gate.go is not preceded by CheckRaw in the same function", call)
		}
	}
}
