package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReasonixConfigIsolation verifies that only the global config and secrets
// are copied from ~/.reasonix, and that projects/ and sessions/ (which hold
// cross-project session data) are left behind. The current project's sessions
// are live-bound at run time by RunSandbox instead.
func TestReasonixConfigIsolation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), ".reasonix")

	// Files that must be copied.
	writeFile(t, filepath.Join(src, "config.toml"), "# reasonix config")
	writeFile(t, filepath.Join(src, ".env"), "DEEPSEEK_API_KEY=sk-test")

	// Cross-project session data that must NOT leak.
	writeFile(t, filepath.Join(src, "projects", "-home-joe-src-flar", "sessions", "s.jsonl"), `{"role":"user"}`)
	writeFile(t, filepath.Join(src, "projects", "-home-joe-src-other", "sessions", "o.jsonl"), `{"role":"user"}`)
	writeFile(t, filepath.Join(src, "sessions", "subagents", "sa_meta.json"), `{}`)

	// PrepareConfigDir needs the full agent switch; test the copy directly.
	if err := CopyDirExcept(src, dst, reasonixSkipCopy); err != nil {
		t.Fatalf("CopyDirExcept: %v", err)
	}

	// Config files must exist.
	mustExist(t, dst, "config.toml")
	mustExist(t, dst, ".env")

	// Session data must NOT exist.
	mustAbsent(t, dst, "projects")
	mustAbsent(t, dst, "sessions")
}

// TestReasonixSlugMatchesClaude verifies that reasonix's project-path encoding
// matches the existing claudeProjectSlug, so we can safely reuse it.
func TestReasonixSlugMatchesClaude(t *testing.T) {
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

	// Verify the slug matches the actual on-disk reasonix directories.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home: %v", err)
	}
	projDir := filepath.Join(home, ".reasonix", "projects")
	entries, err := os.ReadDir(projDir)
	if err != nil {
		t.Skipf("cannot read reasonix projects dir: %v", err)
	}
	// Sanity: at least one project directory should match our slug function
	// when we know the workspace. This test runs from within the flar repo,
	// so -home-joe-src-flar should be present.
	found := false
	for _, e := range entries {
		if e.Name() == "-home-joe-src-flar" {
			found = true
			break
		}
	}
	if !found {
		t.Log("warning: -home-joe-src-flar not found in ~/.reasonix/projects/ (may be OK if reasonix hasn't run here)")
	}
}
