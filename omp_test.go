package main

import (
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// ompSessionHeader returns a JSON session header line for the given cwd.
func ompSessionHeader(cwd string) string {
	return `{"type":"session","version":3,"id":"abc123","timestamp":"2026-01-01T00:00:00Z","cwd":"` + cwd + `","title":"test"}`
}

// ompSessionFileWithTitleSlot returns a JSONL file content with a fixed-width
// title slot (newer omp format) followed by the session header and a message.
func ompSessionFileWithTitleSlot(cwd string) string {
	// The title slot is a 256-byte JSON object with type "title".
	titleSlot := `{"type":"title","title":"test session","source":"auto"}`
	// Pad to 256 bytes to simulate the fixed-width slot.
	for len(titleSlot) < 256 {
		titleSlot += " "
	}
	header := ompSessionHeader(cwd)
	msg := `{"type":"message","id":"msg1","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`
	return titleSlot + "\n" + header + "\n" + msg + "\n"
}

// buildOmpHostStore creates a fake ~/.omp/agent directory with sessions for
// two workspaces and a history.db mixing both. Returns hostHome.
func buildOmpHostStore(t *testing.T, wsA, wsB string) string {
	t.Helper()
	home := t.TempDir()
	store := filepath.Join(home, ".omp", "agent")

	// Create session directories. omp uses project-scoped subdirectories, but
	// a single directory may contain sessions from multiple projects (e.g.,
	// after a cwd migration). We create two directories: one for wsA and one
	// for wsB, plus a mixed directory with sessions from both.
	sessionsDir := filepath.Join(store, "sessions")

	// Directory for wsA sessions (legacy path-based encoding).
	wsADir := filepath.Join(sessionsDir, "-home-joe-src-projA")
	writeOmpFile(t, filepath.Join(wsADir, "20260101T000000_abc123.jsonl"), ompSessionHeader(wsA))
	// wsA session with title slot (newer format).
	writeOmpFile(t, filepath.Join(wsADir, "20260101T010000_def456.jsonl"), ompSessionFileWithTitleSlot(wsA))

	// Directory for wsB sessions.
	wsBDir := filepath.Join(sessionsDir, "-home-joe-src-projB")
	writeOmpFile(t, filepath.Join(wsBDir, "20260101T000000_ghi789.jsonl"), ompSessionHeader(wsB))

	// Mixed directory: contains sessions from both projects (simulates a
	// shared directory after cwd migration).
	mixedDir := filepath.Join(sessionsDir, "-home-joe-src-mixed")
	writeOmpFile(t, filepath.Join(mixedDir, "20260101T000000_a1.jsonl"), ompSessionHeader(wsA))
	writeOmpFile(t, filepath.Join(mixedDir, "20260101T010000_b1.jsonl"), ompSessionHeader(wsB))

	// Create history.db with entries for both workspaces.
	initOmpHistoryDB(t, filepath.Join(store, "history.db"), wsA, wsB)

	return home
}

// writeOmpFile writes content to path, creating parent directories.
func writeOmpFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// initOmpHistoryDB creates a history.db with the omp history schema and
// inserts rows for two different cwds.
func initOmpHistoryDB(t *testing.T, path, wsA, wsB string) {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create the history table as omp does: (id, prompt, created_at, cwd, session_id)
	_, err = db.Exec(`
		CREATE TABLE history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			prompt TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			cwd TEXT NOT NULL,
			session_id TEXT
		);
		CREATE VIRTUAL TABLE history_fts USING fts5(prompt, content='history', content_rowid='id');
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert history rows for both workspaces.
	_, err = db.Exec(`
		INSERT INTO history (prompt, created_at, cwd, session_id) VALUES
			('prompt A1', 1000, ?, 'sess-a1'),
			('prompt A2', 2000, ?, 'sess-a2'),
			('prompt B1', 3000, ?, 'sess-b1');
	`, wsA, wsA, wsB)
	if err != nil {
		t.Fatal(err)
	}

	// Populate the FTS index.
	_, err = db.Exec(`INSERT INTO history_fts(rowid, prompt) SELECT id, prompt FROM history`)
	if err != nil {
		t.Fatal(err)
	}
}

// TestOmpSessionMatchesCwd verifies that ompSessionMatchesCwd correctly
// identifies sessions belonging to a given workspace, including the newer
// title-slot format.
func TestOmpSessionMatchesCwd(t *testing.T) {
	wsA, wsB := "/home/joe/src/projA", "/home/joe/src/projB"

	// Old format: header is the first line.
	oldFormat := ompSessionHeader(wsA)
	if !ompSessionMatchesCwd(writeTempFile(t, oldFormat), wsA) {
		t.Error("old format: expected session to match wsA")
	}
	if ompSessionMatchesCwd(writeTempFile(t, oldFormat), wsB) {
		t.Error("old format: session for wsA should not match wsB")
	}

	// New format: title slot is the first line, header is the second.
	newFormat := ompSessionFileWithTitleSlot(wsA)
	if !ompSessionMatchesCwd(writeTempFile(t, newFormat), wsA) {
		t.Error("new format: expected session to match wsA")
	}
	if ompSessionMatchesCwd(writeTempFile(t, newFormat), wsB) {
		t.Error("new format: session for wsA should not match wsB")
	}

	// Non-session file (no "session" type header).
	nonSession := `{"type":"message","id":"m1","parentId":null,"timestamp":"x","message":{"role":"user","content":[]}}`
	if ompSessionMatchesCwd(writeTempFile(t, nonSession), wsA) {
		t.Error("non-session file should not match any cwd")
	}
}

// writeTempFile writes content to a temp file and returns its path.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "omp-test-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// TestPrepareOmpStoreScoping is the security-critical test: the scoped store
// for workspace A must contain A's sessions and history, and none of B's.
func TestPrepareOmpStoreScoping(t *testing.T) {
	wsA, wsB := "/home/joe/src/projA", "/home/joe/src/projB"
	home := buildOmpHostStore(t, wsA, wsB)

	store, err := prepareOmpStore(home, wsA)
	if err != nil {
		t.Fatal(err)
	}

	mustExist := func(rel string) {
		if _, err := os.Stat(filepath.Join(store, rel)); err != nil {
			t.Errorf("expected %s in scoped store: %v", rel, err)
		}
	}
	mustAbsent := func(rel string) {
		if _, err := os.Stat(filepath.Join(store, rel)); err == nil {
			t.Errorf("%s must NOT be in scoped store (cross-project leak)", rel)
		}
	}

	// A's sessions are present (from both the wsA-only dir and the mixed dir).
	mustExist(filepath.Join("sessions", "-home-joe-src-projA", "20260101T000000_abc123.jsonl"))
	mustExist(filepath.Join("sessions", "-home-joe-src-projA", "20260101T010000_def456.jsonl"))
	mustExist(filepath.Join("sessions", "-home-joe-src-mixed", "20260101T000000_a1.jsonl"))

	// B's sessions are invisible.
	mustAbsent(filepath.Join("sessions", "-home-joe-src-projB", "20260101T000000_ghi789.jsonl"))
	mustAbsent(filepath.Join("sessions", "-home-joe-src-mixed", "20260101T010000_b1.jsonl"))

	// History DB: only wsA rows should remain.
	dbPath := filepath.Join(store, "history.db")
	db, err := openSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history WHERE cwd = ?`, wsA).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 history rows for wsA, got %d", count)
	}

	var bCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history WHERE cwd = ?`, wsB).Scan(&bCount); err != nil {
		t.Fatal(err)
	}
	if bCount != 0 {
		t.Errorf("expected 0 history rows for wsB, got %d (cross-project leak)", bCount)
	}

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("expected 2 total history rows, got %d", total)
	}
}

// TestPrepareOmpStoreSeedsOnce verifies that re-invocation does not clobber
// data the user continued inside flar after the initial seed.
func TestPrepareOmpStoreSeedsOnce(t *testing.T) {
	wsA := "/home/joe/src/projA"
	home := buildOmpHostStore(t, wsA, "/home/joe/src/projB")

	store, err := prepareOmpStore(home, wsA)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an in-flar edit to a session file after seeding.
	sessionFile := filepath.Join(store, "sessions", "-home-joe-src-projA", "20260101T000000_abc123.jsonl")
	if err := os.WriteFile(sessionFile, []byte("edited-in-flar"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Also edit the history DB.
	dbPath := filepath.Join(store, "history.db")
	db, err := openSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO history (prompt, created_at, cwd, session_id) VALUES ('in-flar', 9999, ?, 'sess-inflar')`, wsA); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Re-invoke prepareOmpStore; it should not re-seed.
	if _, err := prepareOmpStore(home, wsA); err != nil {
		t.Fatal(err)
	}

	// The in-flar edit should be preserved.
	got, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "edited-in-flar" {
		t.Errorf("second prepareOmpStore re-seeded and clobbered session file; got %q", got)
	}

	// The in-flar history row should be preserved.
	db2, err := openSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var count int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM history WHERE prompt = 'in-flar'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("second prepareOmpStore re-seeded and clobbered in-flar history row")
	}
}

// TestPrepareOmpStoreNoHostStore verifies that prepareOmpStore handles a host
// with no omp agent directory at all.
func TestPrepareOmpStoreNoHostStore(t *testing.T) {
	home := t.TempDir()
	// No ~/.omp/agent directory exists.
	store, err := prepareOmpStore(home, "/some/project")
	if err != nil {
		t.Fatal(err)
	}
	// The store directory and sessions subdirectory should exist.
	if _, err := os.Stat(filepath.Join(store, "sessions")); err != nil {
		t.Errorf("sessions directory should exist: %v", err)
	}
	// history.db should exist (even if empty).
	if _, err := os.Stat(filepath.Join(store, "history.db")); err != nil {
		t.Errorf("history.db should exist: %v", err)
	}
}

// TestFilterOmpHistoryDB verifies that filterOmpHistoryDB correctly filters
// the history database to only the target project's rows.
func TestFilterOmpHistoryDB(t *testing.T) {
	wsA, wsB := "/home/joe/src/projA", "/home/joe/src/projB"
	home := buildOmpHostStore(t, wsA, wsB)
	hostDB := filepath.Join(home, ".omp", "agent", "history.db")

	dstDB := filepath.Join(t.TempDir(), "history.db")
	if err := filterOmpHistoryDB(hostDB, dstDB, wsA); err != nil {
		t.Fatal(err)
	}

	db, err := openSQLite(dstDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("expected 2 history rows after filtering, got %d", count)
	}

	// Verify the remaining rows are for wsA.
	rows, err := db.Query(`SELECT cwd FROM history`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err != nil {
			t.Fatal(err)
		}
		if cwd != wsA {
			t.Errorf("expected cwd %q, got %q", wsA, cwd)
		}
	}
}

// TestFilterOmpHistoryDBFreshHost verifies that filtering a database with no
// matching project produces an empty but valid database.
func TestFilterOmpHistoryDBFreshHost(t *testing.T) {
	wsA := "/home/joe/src/projA"
	home := buildOmpHostStore(t, wsA, "/home/joe/src/projB")
	hostDB := filepath.Join(home, ".omp", "agent", "history.db")

	dstDB := filepath.Join(t.TempDir(), "history.db")
	if err := filterOmpHistoryDB(hostDB, dstDB, "/nonexistent/project"); err != nil {
		t.Fatal(err)
	}

	db, err := openSQLite(dstDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 history rows for non-matching project, got %d", count)
	}
}

// TestFilterOmpHistoryDBNoDatabase verifies that filterOmpHistoryDB handles a
// missing source database gracefully.
func TestFilterOmpHistoryDBNoDatabase(t *testing.T) {
	dstDB := filepath.Join(t.TempDir(), "history.db")
	err := filterOmpHistoryDB("/nonexistent/history.db", dstDB, "/some/project")
	if err == nil {
		t.Error("expected error for missing source database")
	}
}

// TestOmpEnvVarsForProviders verifies that envVarsForAgentOmp forwards only the
// keys for allowlisted providers when OmpAllowedModels is set, and all common
// keys when empty.
func TestOmpEnvVarsForProviders(t *testing.T) {
	// With no allowlist, all common provider keys are forwarded.
	allVars := envVarsForAgentOmp(nil)
	if len(allVars) == 0 {
		t.Error("expected non-zero env vars with no allowlist")
	}
	// Should include common keys.
	allSet := make(map[string]bool)
	for _, v := range allVars {
		allSet[v] = true
	}
	if !allSet["ANTHROPIC_API_KEY"] || !allSet["OPENAI_API_KEY"] || !allSet["GEMINI_API_KEY"] {
		t.Error("expected common provider keys in full env vars")
	}

	// With an allowlist of only "anthropic", only ANTHROPIC_API_KEY and
	// ANTHROPIC_OAUTH_TOKEN should be forwarded.
	anthropicVars := envVarsForAgentOmp([]string{"anthropic"})
	anthropicSet := make(map[string]bool)
	for _, v := range anthropicVars {
		anthropicSet[v] = true
	}
	if !anthropicSet["ANTHROPIC_API_KEY"] || !anthropicSet["ANTHROPIC_OAUTH_TOKEN"] {
		t.Error("expected ANTHROPIC_API_KEY and ANTHROPIC_OAUTH_TOKEN for anthropic-only allowlist")
	}
	if anthropicSet["OPENAI_API_KEY"] {
		t.Error("OPENAI_API_KEY should NOT be forwarded for anthropic-only allowlist")
	}
	if anthropicSet["GEMINI_API_KEY"] {
		t.Error("GEMINI_API_KEY should NOT be forwarded for anthropic-only allowlist")
	}

	// With an allowlist of "anthropic" and "openai", both sets should be present.
	bothVars := envVarsForAgentOmp([]string{"anthropic", "openai"})
	bothSet := make(map[string]bool)
	for _, v := range bothVars {
		bothSet[v] = true
	}
	if !bothSet["ANTHROPIC_API_KEY"] || !bothSet["OPENAI_API_KEY"] {
		t.Error("expected both anthropic and openai keys for dual allowlist")
	}
}

// TestOmpValidateProviders verifies that ompValidateProviders correctly
// identifies known and unknown provider IDs.
func TestOmpValidateProviders(t *testing.T) {
	// Known providers should not appear in the unknown list.
	unknown := ompValidateProviders([]string{"anthropic", "openai", "google"})
	if len(unknown) != 0 {
		t.Errorf("expected no unknown providers, got %v", unknown)
	}

	// Unknown providers should be returned.
	unknown = ompValidateProviders([]string{"anthropic", "fake-provider", "another-fake"})
	if len(unknown) != 2 {
		t.Errorf("expected 2 unknown providers, got %d: %v", len(unknown), unknown)
	}
	if unknown[0] != "fake-provider" || unknown[1] != "another-fake" {
		t.Errorf("expected [fake-provider, another-fake], got %v", unknown)
	}
}

// TestOmpSkipCopy verifies that the ompSkipCopy list correctly identifies the
// paths that should be skipped during config copying.
func TestOmpSkipCopy(t *testing.T) {
	if !ompSkipCopy["sessions"] {
		t.Error("sessions should be in ompSkipCopy")
	}
	if !ompSkipCopy["history.db"] {
		t.Error("history.db should be in ompSkipCopy")
	}
	if !ompSkipCopy["history.db-wal"] {
		t.Error("history.db-wal should be in ompSkipCopy")
	}
	if !ompSkipCopy["history.db-shm"] {
		t.Error("history.db-shm should be in ompSkipCopy")
	}
	if !ompSkipCopy["terminal-sessions"] {
		t.Error("terminal-sessions should be in ompSkipCopy")
	}
	// Safe paths should not be in the skip list.
	if ompSkipCopy["config.yml"] {
		t.Error("config.yml should NOT be in ompSkipCopy")
	}
	if ompSkipCopy["models.yml"] {
		t.Error("models.yml should NOT be in ompSkipCopy")
	}
	if ompSkipCopy["agent.db"] {
		t.Error("agent.db should NOT be in ompSkipCopy")
	}
	if ompSkipCopy["blobs"] {
		t.Error("blobs should NOT be in ompSkipCopy")
	}
}

// TestCopyDirExceptOmpSkips verifies that CopyDirExcept correctly skips the
// cross-project paths when copying omp's config directory.
func TestCopyDirExceptOmpSkips(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	// Safe files that should be copied.
	writeOmpFile(t, filepath.Join(src, "config.yml"), "settings")
	writeOmpFile(t, filepath.Join(src, "models.yml"), "models")
	writeOmpFile(t, filepath.Join(src, "agent.db"), "auth")
	writeOmpFile(t, filepath.Join(src, "blobs", "abc123"), "blob")

	// Cross-project paths that should be skipped.
	writeOmpFile(t, filepath.Join(src, "sessions", "-projA", "sess.jsonl"), "session-a")
	writeOmpFile(t, filepath.Join(src, "sessions", "-projB", "sess.jsonl"), "session-b")
	writeOmpFile(t, filepath.Join(src, "history.db"), "history-a-b")
	writeOmpFile(t, filepath.Join(src, "terminal-sessions", "tty1"), "breadcrumb")

	if err := CopyDirExcept(src, dst, ompSkipCopy); err != nil {
		t.Fatal(err)
	}

	// Safe files should be copied.
	for _, rel := range []string{"config.yml", "models.yml", "agent.db", filepath.Join("blobs", "abc123")} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("%s should be copied: %v", rel, err)
		}
	}

	// Cross-project paths should be skipped.
	for _, rel := range []string{
		"sessions",
		"history.db",
		"terminal-sessions",
	} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err == nil {
			t.Errorf("%s must be skipped (cross-project leak)", rel)
		}
	}
}

// TestOmpAgentDir verifies the omp agent directory resolution, including
// PI_CODING_AGENT_DIR override.
func TestOmpAgentDir(t *testing.T) {
	home := "/home/testuser"

	// Default: ~/.omp/agent
	got := ompAgentDir(home)
	expected := filepath.Join(home, ".omp", "agent")
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}

	// With PI_CODING_AGENT_DIR set.
	t.Setenv("PI_CODING_AGENT_DIR", "/custom/omp/dir")
	got = ompAgentDir(home)
	if got != "/custom/omp/dir" {
		t.Errorf("expected /custom/omp/dir, got %q", got)
	}
}

// TestOmpSessionMatchesCwdEmptyFile verifies that an empty or whitespace-only
// file does not match any cwd.
func TestOmpSessionMatchesCwdEmptyFile(t *testing.T) {
	path := writeTempFile(t, "\n\n")
	if ompSessionMatchesCwd(path, "/any/project") {
		t.Error("empty file should not match any cwd")
	}
}

// TestOmpSessionMatchesCwdInvalidJSON verifies that invalid JSON lines are
// tolerated and do not cause a crash.
func TestOmpSessionMatchesCwdInvalidJSON(t *testing.T) {
	wsA := "/home/joe/src/projA"
	content := "not json at all\n" + ompSessionHeader(wsA) + "\n"
	path := writeTempFile(t, content)
	if !ompSessionMatchesCwd(path, wsA) {
		t.Error("expected session to match wsA despite invalid first line")
	}
}

// TestPrepareOmpStoreEmptySessionsDir verifies that prepareOmpStore creates
// the sessions directory even when the host has no sessions.
func TestPrepareOmpStoreEmptySessionsDir(t *testing.T) {
	home := t.TempDir()
	// Create ~/.omp/agent but no sessions/ directory.
	store := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := prepareOmpStore(home, "/some/project")
	if err != nil {
		t.Fatal(err)
	}

	// Sessions directory should exist in the scoped store.
	if _, err := os.Stat(filepath.Join(result, "sessions")); err != nil {
		t.Errorf("sessions directory should exist: %v", err)
	}
	// History DB should exist (even if empty).
	if _, err := os.Stat(filepath.Join(result, "history.db")); err != nil {
		t.Errorf("history.db should exist: %v", err)
	}
}

// TestOmpEnvVarsForProvidersEmpty verifies that an empty (nil) provider list
// returns all common env vars, since an empty allowlist means "no restriction".
func TestOmpEnvVarsForProvidersEmpty(t *testing.T) {
	vars := envVarsForAgentOmp(nil)
	if len(vars) == 0 {
		t.Error("expected non-zero env vars with nil allowlist")
	}
	// Should include credential vars (empty allowlist = no restriction).
	found := make(map[string]bool)
	for _, v := range vars {
		found[v] = true
	}
	if !found["ANTHROPIC_API_KEY"] || !found["OPENAI_API_KEY"] || !found["GEMINI_API_KEY"] {
		t.Error("expected common credential keys with nil allowlist")
	}
}
