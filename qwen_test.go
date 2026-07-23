package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestQwenConfigIsolation verifies that only the global config is copied from
// ~/.qwen, and that projects/ (which holds per-project session data), tmp/,
// and usage/ are left behind. The current project's sessions are live-bound
// at run time by RunSandbox instead.
func TestQwenConfigIsolation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), ".qwen")

	// Files that must be copied.
	writeFile(t, filepath.Join(src, "settings.json"), `{"env":{"DASHSCOPE_API_KEY":"sk-test"}}`)
	writeFile(t, filepath.Join(src, "output-language.md"), "# Output language")
	writeFile(t, filepath.Join(src, "installation_id"), "test-id")
	writeFile(t, filepath.Join(src, "extensions", "ext.json"), "{}")
	writeFile(t, filepath.Join(src, "skills", "skill.json"), "{}")
	writeFile(t, filepath.Join(src, "memories", "user", "role.md"), "---\nname: role\n---")

	// Cross-project session data that must NOT leak.
	writeFile(t, filepath.Join(src, "projects", "-home-joe-src-flar", "chats", "s.jsonl"), `{"role":"user"}`)
	writeFile(t, filepath.Join(src, "projects", "-home-joe-src-other", "chats", "o.jsonl"), `{"role":"user"}`)
	writeFile(t, filepath.Join(src, "tmp", "abc123", "logs.json"), "{}")
	writeFile(t, filepath.Join(src, "usage", "token-usage-2026-07.jsonl"), "{}")

	if err := CopyDirExcept(src, dst, qwenSkipCopy); err != nil {
		t.Fatalf("CopyDirExcept: %v", err)
	}

	// Config files must exist.
	mustExist(t, dst, "settings.json")
	mustExist(t, dst, "output-language.md")
	mustExist(t, dst, "installation_id")
	mustExist(t, dst, filepath.Join("extensions", "ext.json"))
	mustExist(t, dst, filepath.Join("skills", "skill.json"))
	mustExist(t, dst, filepath.Join("memories", "user", "role.md"))

	// Session data must NOT exist.
	mustAbsent(t, dst, "projects")
	mustAbsent(t, dst, "tmp")
	mustAbsent(t, dst, "usage")
}

// TestQwenSlugMatchesClaude verifies that qwen's project-path encoding matches
// the existing claudeProjectSlug, so we can safely reuse it.
func TestQwenSlugMatchesClaude(t *testing.T) {
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

	// Verify the slug matches the actual on-disk qwen directories.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home: %v", err)
	}
	projDir := filepath.Join(home, ".qwen", "projects")
	entries, err := os.ReadDir(projDir)
	if err != nil {
		t.Skipf("cannot read qwen projects dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "-home-joe-src-flar" {
			found = true
			break
		}
	}
	if !found {
		t.Log("warning: -home-joe-src-flar not found in ~/.qwen/projects/ (may be OK if qwen hasn't run here)")
	}
}
