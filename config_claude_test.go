package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeProjectSlug(t *testing.T) {
	cases := map[string]string{
		"/home/joe/src/flar":                       "-home-joe-src-flar",
		"/home/joe/.tandem/p1000.mvpn.savioke.com": "-home-joe--tandem-p1000-mvpn-savioke-com",
		"/home/joe/src/every.camp":                 "-home-joe-src-every-camp",
	}
	for in, want := range cases {
		if got := claudeProjectSlug(in); got != want {
			t.Errorf("claudeProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCopyClaudeConfigIsolation verifies that only allowlisted entries are copied,
// and that other projects' history and unrelated cross-session data are left behind.
// The current project's transcripts are intentionally NOT copied here; RunSandbox
// live-binds them from the host instead.
func TestCopyClaudeConfigIsolation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), ".claude")

	// Allowlisted entries that must survive.
	writeFile(t, filepath.Join(src, ".credentials.json"), "token")
	writeFile(t, filepath.Join(src, "settings.json"), "{}")
	writeFile(t, filepath.Join(src, "CLAUDE.md"), "instructions")
	writeFile(t, filepath.Join(src, "plugins", "p.json"), "{}")

	// Cross-project / cross-session data that must NOT leak.
	writeFile(t, filepath.Join(src, "history.jsonl"), "other-project prompts")
	writeFile(t, filepath.Join(src, "sessions", "s.json"), "secret")
	writeFile(t, filepath.Join(src, "shell-snapshots", "snap.sh"), "env")
	writeFile(t, filepath.Join(src, "projects", "-home-joe-src-other", "t.jsonl"), "OTHER transcript")

	// Current project's transcripts exist on the host but must NOT be copied; they
	// are live-bound by RunSandbox instead.
	proj := "/home/joe/src/flar"
	writeFile(t, filepath.Join(src, "projects", claudeProjectSlug(proj), "t.jsonl"), "flar transcript")

	if err := copyClaudeConfig(src, dst, proj); err != nil {
		t.Fatalf("copyClaudeConfig: %v", err)
	}

	mustExist(t, dst, ".credentials.json")
	mustExist(t, dst, "settings.json")
	mustExist(t, dst, "CLAUDE.md")
	mustExist(t, dst, filepath.Join("plugins", "p.json"))

	mustAbsent(t, dst, "history.jsonl")
	mustAbsent(t, dst, filepath.Join("sessions", "s.json"))
	mustAbsent(t, dst, filepath.Join("shell-snapshots", "snap.sh"))
	mustAbsent(t, dst, filepath.Join("projects", "-home-joe-src-other", "t.jsonl"))
	mustAbsent(t, dst, filepath.Join("projects", "-home-joe-src-other"))
	mustAbsent(t, dst, "projects") // no transcripts copied at all now
}

// TestCopyClaudeJSONFiltersProjects verifies the per-project map is reduced to the
// current project while all other top-level fields are preserved.
func TestCopyClaudeJSONFiltersProjects(t *testing.T) {
	src := filepath.Join(t.TempDir(), ".claude.json")
	dst := filepath.Join(t.TempDir(), ".claude.json")

	proj := "/home/joe/src/flar"
	input := map[string]any{
		"oauthAccount":           map[string]any{"id": "abc"},
		"hasCompletedOnboarding": true,
		"projects": map[string]any{
			proj:                   map[string]any{"history": []string{"flar prompt"}},
			"/home/joe/src/webmin": map[string]any{"history": []string{"webmin prompt"}},
			"/home/joe/src/charly": map[string]any{"history": []string{"charly prompt"}},
		},
	}
	b, _ := json.Marshal(input)
	writeFile(t, src, string(b))

	if err := copyClaudeJSON(src, dst, proj); err != nil {
		t.Fatalf("copyClaudeJSON: %v", err)
	}

	var out map[string]json.RawMessage
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse dst: %v", err)
	}

	// Non-project fields preserved.
	if _, ok := out["oauthAccount"]; !ok {
		t.Error("oauthAccount was dropped")
	}
	if _, ok := out["hasCompletedOnboarding"]; !ok {
		t.Error("hasCompletedOnboarding was dropped")
	}

	var projects map[string]json.RawMessage
	if err := json.Unmarshal(out["projects"], &projects); err != nil {
		t.Fatalf("parse projects: %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("projects map has %d entries, want 1", len(projects))
	}
	if _, ok := projects[proj]; !ok {
		t.Errorf("current project %q missing from filtered map", proj)
	}
	for _, other := range []string{"/home/joe/src/webmin", "/home/joe/src/charly"} {
		if _, ok := projects[other]; ok {
			t.Errorf("other project %q leaked into filtered map", other)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustExist(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
		t.Errorf("expected %s to exist, but: %v", rel, err)
	}
}

func mustAbsent(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
		t.Errorf("expected %s to be absent, but it exists", rel)
	}
}
