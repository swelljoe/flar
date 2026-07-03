package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func initCodexTestDB(t *testing.T, path string) {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT, cwd TEXT, title TEXT);
		CREATE TABLE thread_dynamic_tools (thread_id TEXT, position INTEGER, name TEXT, PRIMARY KEY(thread_id, position));
		CREATE TABLE thread_spawn_edges (parent_thread_id TEXT, child_thread_id TEXT);
		CREATE TABLE agent_jobs (id TEXT);
		CREATE TABLE agent_job_items (id TEXT);
		CREATE TABLE remote_control_enrollments (id TEXT);
		CREATE TABLE external_agent_config_imports (id TEXT);
		CREATE TABLE backfill_state (id TEXT);
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func writeCodexSession(t *testing.T, root, rel, id, cwd, body string) string {
	t.Helper()
	path := filepath.Join(root, "sessions", rel)
	writeFile(t, path, fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q}}`, id, cwd)+"\n"+body)
	return path
}

func TestPrepareCodexStoreSeedsOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	host := filepath.Join(home, ".codex")
	configSrc := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(host, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(configSrc, "auth.json"), "auth")
	initCodexTestDB(t, filepath.Join(host, codexStateDB))
	db, _ := openSQLite(filepath.Join(host, codexStateDB))
	_, err := db.Exec(`INSERT INTO threads VALUES ('a', 'a.jsonl', '/work/a', 'A'), ('b', 'b.jsonl', '/work/b', 'B'); INSERT INTO thread_dynamic_tools VALUES ('a', 0, 'ta'), ('b', 0, 'tb')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	writeCodexSession(t, host, "2026/01/a.jsonl", "a", "/work/a", "old-a\n")
	writeCodexSession(t, host, "2026/01/b.jsonl", "b", "/work/b", "old-b\n")
	writeFile(t, filepath.Join(host, "history.jsonl"), `{"session_id":"a","text":"A"}`+"\n"+`{"session_id":"b","text":"B"}`+"\n")

	scoped, err := prepareCodexStore(home, "/work/a", configSrc)
	if err != nil {
		t.Fatal(err)
	}
	mustExist(t, scoped, "auth.json")
	mustExist(t, scoped, filepath.Join("sessions", "2026/01/a.jsonl"))
	mustAbsent(t, scoped, filepath.Join("sessions", "2026/01/b.jsonl"))
	history, _ := os.ReadFile(filepath.Join(scoped, "history.jsonl"))
	if strings.Contains(string(history), `"b"`) || !strings.Contains(string(history), `"a"`) {
		t.Fatalf("history not scoped: %s", history)
	}
	db, _ = openSQLite(filepath.Join(scoped, codexStateDB))
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM threads WHERE cwd <> '/work/a'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unscoped threads remain: count=%d err=%v", count, err)
	}
	db.Close()

	// A sandbox-created session remains in the shadow store.
	writeCodexSession(t, scoped, "2026/02/c.jsonl", "c", "/work/a", "new-c\n")
	db, _ = openSQLite(filepath.Join(scoped, codexStateDB))
	_, err = db.Exec(`INSERT INTO threads VALUES ('c', 'c.jsonl', '/work/a', 'C')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	mustAbsent(t, host, filepath.Join("sessions", "2026/02/c.jsonl"))

	// Later host-side sessions are not imported after the one-time seed.
	writeCodexSession(t, host, "2026/03/d.jsonl", "d", "/work/a", "host-d\n")
	db, _ = openSQLite(filepath.Join(host, codexStateDB))
	_, err = db.Exec(`INSERT INTO threads VALUES ('d', 'd.jsonl', '/work/a', 'D')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	again, err := prepareCodexStore(home, "/work/a", configSrc)
	if err != nil || again != scoped {
		t.Fatalf("second prepare = %q, %v; want %q", again, err, scoped)
	}
	mustAbsent(t, scoped, filepath.Join("sessions", "2026/03/d.jsonl"))
	mustExist(t, scoped, filepath.Join("sessions", "2026/02/c.jsonl"))
}

func TestFlarStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/lib/user-state")
	if got, want := flarStateDir("/home/test"), "/var/lib/user-state/flar"; got != want {
		t.Fatalf("flarStateDir = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	if got, want := flarStateDir("/home/test"), "/home/test/.local/state/flar"; got != want {
		t.Fatalf("flarStateDir with invalid XDG value = %q, want %q", got, want)
	}
}
