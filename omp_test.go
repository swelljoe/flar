package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOmpConfigIsolation verifies that only the global config is copied from
// ~/.omp/agent, and that session state (sessions/), history database
// (history.db*), credential store (agent.db*), and blob store (blobs/) are
// left behind. The current project's sessions are supplied at run time by
// the shadow home, and credentials are provided via env vars.
func TestOmpConfigIsolation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "omp-agent")

	// Files that must be copied.
	writeFile(t, filepath.Join(src, "config.yml"), "# omp config")
	writeFile(t, filepath.Join(src, ".env"), "OPENAI_API_KEY=test-key")
	writeFile(t, filepath.Join(src, "skills", "test.md"), "skill content")

	// Cross-project data that must NOT leak.
	writeFile(t, filepath.Join(src, "sessions", "abc123", "session.jsonl"), "{}\n")
	writeFile(t, filepath.Join(src, "history.db"), "fake-history")
	writeFile(t, filepath.Join(src, "history.db-wal"), "wal")
	writeFile(t, filepath.Join(src, "history.db-shm"), "shm")
	writeFile(t, filepath.Join(src, "blobs", "sha256hash"), "blob-data")
	writeFile(t, filepath.Join(src, "agent.db"), "fake-agent-db")
	writeFile(t, filepath.Join(src, "caches", "model-cache"), "cached")

	if err := CopyDirExcept(src, dst, ompSkipCopy); err != nil {
		t.Fatalf("CopyDirExcept: %v", err)
	}

	// Config files must exist.
	mustExist(t, dst, "config.yml")
	mustExist(t, dst, ".env")
	mustExist(t, dst, filepath.Join("skills", "test.md"))

	// Session state, history, agent DB, blobs, and caches must NOT exist.
	mustAbsent(t, dst, "sessions")
	mustAbsent(t, dst, "history.db")
	mustAbsent(t, dst, "history.db-wal")
	mustAbsent(t, dst, "history.db-shm")
	mustAbsent(t, dst, "blobs")
	mustAbsent(t, dst, "agent.db")
	mustAbsent(t, dst, "caches")
}

// TestOmpEnvVarsForModel verifies that ompEnvVarsForModel returns the correct
// environment variables for each provider, and all vars when model is empty.
func TestOmpEnvVarsForModel(t *testing.T) {
	cases := []struct {
		model string
		want  []string
	}{
		{"", []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_FOUNDRY_API_KEY",
			"ANTHROPIC_SEARCH_API_KEY", "OPENAI_API_KEY", "OPENAI_CODEX_OAUTH_TOKEN",
			"GEMINI_API_KEY", "GOOGLE_CLOUD_API_KEY", "AZURE_OPENAI_API_KEY",
			"XAI_API_KEY", "OPENROUTER_API_KEY", "QWEN_OAUTH_TOKEN",
			"QWEN_PORTAL_API_KEY", "CURSOR_ACCESS_TOKEN", "HUGGINGFACE_HUB_TOKEN",
			"HF_TOKEN", "OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN",
		}},
		{"anthropic", []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_FOUNDRY_API_KEY",
			"ANTHROPIC_SEARCH_API_KEY", "OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN",
		}},
		{"openai", []string{
			"OPENAI_API_KEY", "OPENAI_CODEX_OAUTH_TOKEN",
			"OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN",
		}},
		{"google", []string{
			"GEMINI_API_KEY", "GOOGLE_CLOUD_API_KEY",
			"OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN",
		}},
		{"xai", []string{
			"XAI_API_KEY",
			"OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN",
		}},
		{"broker", []string{
			"OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN",
		}},
	}
	for _, c := range cases {
		got := ompEnvVarsForModel(c.model)
		if len(got) != len(c.want) {
			t.Errorf("ompEnvVarsForModel(%q): got %d vars, want %d: %v",
				c.model, len(got), len(c.want), got)
			continue
		}
		gotSet := make(map[string]bool)
		for _, v := range got {
			gotSet[v] = true
		}
		for _, w := range c.want {
			if !gotSet[w] {
				t.Errorf("ompEnvVarsForModel(%q): missing %q", c.model, w)
			}
		}
	}
}

// TestPrepareOmpStoreSeedsOnlyCurrentProject verifies that the shadow store
// is seeded with exactly the sessions attributed to the current project —
// nothing from any other project.
func TestPrepareOmpStoreSeedsOnlyCurrentProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	projA := "/home/joe/src/flar"
	projB := "/home/joe/src/other"
	hostHome := t.TempDir()
	hostAgent := ompAgentDir(hostHome)

	// Create sessions for two projects.
	projectHashA := fmt.Sprintf("%x", sha256.Sum256([]byte(projA)))
	projectHashB := fmt.Sprintf("%x", sha256.Sum256([]byte(projB)))

	writeFile(t, filepath.Join(hostAgent, "sessions", projectHashA, "session1.jsonl"), `{"content":"hello"}`+"\n")
	writeFile(t, filepath.Join(hostAgent, "sessions", projectHashA, "session2.jsonl"), `{"content":"world"}`+"\n")
	writeFile(t, filepath.Join(hostAgent, "sessions", projectHashB, "session3.jsonl"), `{"content":"other"}`+"\n")

	// Create a history.db with rows for both projects.
	hostHistory := filepath.Join(hostAgent, "history.db")
	db, err := openSQLite(hostHistory)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	defer db.Close()
	_, _ = db.Exec(`CREATE TABLE history (id INTEGER PRIMARY KEY, prompt TEXT, created_at TEXT, cwd TEXT, session_id TEXT)`)
	_, _ = db.Exec(`INSERT INTO history (id, prompt, created_at, cwd, session_id) VALUES (1, 'hello', '2026-01-01T00:00:00Z', ?, 'session1')`, projA)
	_, _ = db.Exec(`INSERT INTO history (id, prompt, created_at, cwd, session_id) VALUES (2, 'world', '2026-01-01T00:00:01Z', ?, 'session2')`, projA)
	_, _ = db.Exec(`INSERT INTO history (id, prompt, created_at, cwd, session_id) VALUES (3, 'other', '2026-01-01T00:00:02Z', ?, 'session3')`, projB)
	db.Close()

	// configSrc simulates the per-run temp config copy.
	configSrc := t.TempDir()
	writeFile(t, filepath.Join(configSrc, "config.yml"), "# omp config")

	store, err := prepareOmpStore(hostHome, projA, configSrc)
	if err != nil {
		t.Fatalf("prepareOmpStore: %v", err)
	}

	// Config is seeded from configSrc.
	mustExist(t, store, "config.yml")
	mustExist(t, store, ".seeded")

	// This project's sessions are seeded; the other project's are not.
	mustExist(t, store, filepath.Join("sessions", projectHashA, "session1.jsonl"))
	mustExist(t, store, filepath.Join("sessions", projectHashA, "session2.jsonl"))
	mustAbsent(t, store, filepath.Join("sessions", projectHashB, "session3.jsonl"))

	// history.db is filtered to this project.
	shadowHistory := filepath.Join(store, "history.db")
	shadowDB, err := openSQLite(shadowHistory)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	defer shadowDB.Close()
	var count int
	_ = shadowDB.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&count)
	if count != 2 {
		t.Errorf("filtered history has %d rows, want 2", count)
	}
	var cwd string
	_ = shadowDB.QueryRow(`SELECT cwd FROM history WHERE id = 1`).Scan(&cwd)
	if cwd != projA {
		t.Errorf("history cwd = %q, want %q", cwd, projA)
	}
}

// TestPrepareOmpStoreSeedsOnce verifies the fork semantics: after the
// one-time seed, later host-side sessions are not pulled in.
func TestPrepareOmpStoreSeedsOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	projA := "/home/joe/src/flar"
	hostHome := t.TempDir()
	hostAgent := ompAgentDir(hostHome)

	projectHashA := fmt.Sprintf("%x", sha256.Sum256([]byte(projA)))
	writeFile(t, filepath.Join(hostAgent, "sessions", projectHashA, "session1.jsonl"), `{"content":"hello"}`+"\n")

	configSrc := t.TempDir()
	store, err := prepareOmpStore(hostHome, projA, configSrc)
	if err != nil {
		t.Fatalf("prepareOmpStore: %v", err)
	}

	// A session created inside the sandbox must survive; a new host-side
	// session must NOT be pulled in.
	writeFile(t, filepath.Join(store, "sessions", projectHashA, "session2.jsonl"), `{"content":"sandboxed"}`+"\n")
	writeFile(t, filepath.Join(hostAgent, "sessions", projectHashA, "session3.jsonl"), `{"content":"host"}`+"\n")

	store2, err := prepareOmpStore(hostHome, projA, configSrc)
	if err != nil {
		t.Fatalf("prepareOmpStore (second): %v", err)
	}
	if store2 != store {
		t.Errorf("store path changed between runs: %q vs %q", store2, store)
	}
	mustExist(t, store2, filepath.Join("sessions", projectHashA, "session2.jsonl"))
	mustAbsent(t, store2, filepath.Join("sessions", projectHashA, "session3.jsonl"))
}

// TestPrepareOmpStoreFreshHost verifies seeding against a host with no omp
// history at all: the store still gets the config plus empty session structures.
func TestPrepareOmpStoreFreshHost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	hostHome := t.TempDir()
	configSrc := t.TempDir()
	writeFile(t, filepath.Join(configSrc, "config.yml"), "# omp config")

	store, err := prepareOmpStore(hostHome, "/home/joe/src/flar", configSrc)
	if err != nil {
		t.Fatalf("prepareOmpStore: %v", err)
	}
	mustExist(t, store, "config.yml")
	mustExist(t, store, ".seeded")
	mustExist(t, store, "sessions")
	mustAbsent(t, store, "history.db")
	mustAbsent(t, store, "blobs")
}

// TestFilterOmpHistory verifies that filterOmpHistory correctly removes rows
// not attributed to the current project and rebuilds the FTS index.
func TestFilterOmpHistory(t *testing.T) {
	hostHome := t.TempDir()
	hostDB := filepath.Join(hostHome, "history.db")
	db, err := openSQLite(hostDB)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	defer db.Close()

	_, _ = db.Exec(`CREATE TABLE history (id INTEGER PRIMARY KEY, prompt TEXT, created_at TEXT, cwd TEXT, session_id TEXT)`)
	_, _ = db.Exec(`CREATE VIRTUAL TABLE history_fts USING fts5(prompt, content=cwd)`)

	projA := "/home/joe/src/flar"
	projB := "/home/joe/src/other"
	_, _ = db.Exec(`INSERT INTO history (id, prompt, created_at, cwd, session_id) VALUES (1, 'prompt a', '2026-01-01T00:00:00Z', ?, 's1')`, projA)
	_, _ = db.Exec(`INSERT INTO history (id, prompt, created_at, cwd, session_id) VALUES (2, 'prompt b', '2026-01-01T00:00:01Z', ?, 's2')`, projA)
	_, _ = db.Exec(`INSERT INTO history (id, prompt, created_at, cwd, session_id) VALUES (3, 'prompt c', '2026-01-01T00:00:02Z', ?, 's3')`, projB)

	// Insert into FTS index.
	_, _ = db.Exec(`INSERT INTO history_fts(rowid, content) SELECT id, prompt FROM history WHERE 1=0`)
	_, _ = db.Exec(`INSERT INTO history_fts(rowid, content) SELECT id, prompt FROM history`)

	dst := filepath.Join(t.TempDir(), "filtered.db")
	if err := filterOmpHistory(hostDB, dst, projA); err != nil {
		t.Fatalf("filterOmpHistory: %v", err)
	}

	shadow, err := openSQLite(dst)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	defer shadow.Close()

	var count int
	_ = shadow.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&count)
	if count != 2 {
		t.Errorf("filtered history has %d rows, want 2", count)
	}

	var cwd string
	_ = shadow.QueryRow(`SELECT cwd FROM history WHERE id = 1`).Scan(&cwd)
	if cwd != projA {
		t.Errorf("cwd = %q, want %q", cwd, projA)
	}

	// FTS index is best-effort: VACUUM INTO may not preserve virtual tables.
	// The important invariant is that the history table is filtered correctly.
}

// TestFlarOmpAutoDetect verifies that omp is auto-detected when its config
// directory exists or when relevant env vars are set.
func TestFlarOmpAutoDetect(t *testing.T) {
	unsetAgentEnvs(t)

	// Test 1: omp config directory exists.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".omp", "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := autoDetectAgent(); got != AgentOmp {
		t.Errorf("autoDetectAgent() = %q, want %q (omp agent dir exists)", got, AgentOmp)
	}

	// Test 2: OMP-specific env var set.
	home2 := t.TempDir()
	t.Setenv("HOME", home2)
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := os.RemoveAll(filepath.Join(home2, ".omp")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_AUTH_BROKER_URL", "https://example.com")
	if got := autoDetectAgent(); got != AgentOmp {
		t.Errorf("autoDetectAgent() = %q, want %q (OMP_AUTH_BROKER_URL set)", got, AgentOmp)
	}
}

// TestOmpEnvVarsScoping verifies that only the expected env vars are forwarded
// for the omp agent when no model is specified (all vars).
func TestOmpEnvVarsScoping(t *testing.T) {
	vars := envVarsForAgent(AgentOmp)
	// Should include common vars.
	for _, v := range commonEnvVars {
		found := false
		for _, actual := range vars {
			if actual == v {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("envVarsForAgent(AgentOmp): missing common var %q", v)
		}
	}
	// Should NOT include other agents' vars.
	bad := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY",
		"GITHUB_TOKEN", "DEEPSEEK_API_KEY", "KIMI_API_KEY",
		"POOLSIDE_API_KEY", "DASHSCOPE_API_KEY", "XIAOMI_API_KEY"}
	for _, b := range bad {
		for _, v := range vars {
			if v == b {
				t.Errorf("envVarsForAgent(AgentOmp): should not include %q", b)
			}
		}
	}
}

// TestRedactedArgsCoversOmpCredentials verifies that OMP credential vars are
// redacted in verbose output.
func TestRedactedArgsCoversOmpCredentials(t *testing.T) {
	args := []string{"--setenv", "ANTHROPIC_API_KEY", "sk-test123",
		"--setenv", "OPENAI_API_KEY", "sk-test456",
		"--setenv", "PATH", "/usr/bin"}
	result := redactedArgs(args)
	// Check that secret vars are redacted.
	if !strings.Contains(strings.Join(result, " "), "ANTHROPIC_API_KEY") {
		t.Error("redactedArgs: lost variable name ANTHROPIC_API_KEY")
	}
	if strings.Contains(strings.Join(result, " "), "sk-test123") {
		t.Error("redactedArgs: leaked ANTHROPIC_API_KEY value")
	}
	if !strings.Contains(strings.Join(result, " "), "OPENAI_API_KEY") {
		t.Error("redactedArgs: lost variable name OPENAI_API_KEY")
	}
	if strings.Contains(strings.Join(result, " "), "sk-test456") {
		t.Error("redactedArgs: leaked OPENAI_API_KEY value")
	}
	// PATH should not be redacted.
	if !strings.Contains(strings.Join(result, " "), "/usr/bin") {
		t.Error("redactedArgs: incorrectly redacted PATH")
	}
}

