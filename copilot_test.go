package main

import (
	"os"
	"path/filepath"
	"testing"
)

func buildCopilotHostStore(t *testing.T, wsA, wsB string) string {
	t.Helper()

	home := t.TempDir()
	root := filepath.Join(home, ".copilot")
	writeFile(t, filepath.Join(root, "settings.json"), `{"theme":"dark"}`)
	writeFile(t, filepath.Join(root, "copilot-instructions.md"), "be careful")

	db, err := openSQLite(filepath.Join(root, copilotSessionStoreDB))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			cwd TEXT,
			repository TEXT,
			host_type TEXT,
			branch TEXT,
			summary TEXT,
			created_at TEXT DEFAULT (datetime('now')),
			updated_at TEXT DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX idx_sessions_cwd ON sessions(cwd)`,
		`CREATE TABLE turns (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			turn_index INTEGER NOT NULL,
			user_message TEXT,
			assistant_response TEXT,
			timestamp TEXT DEFAULT (datetime('now')),
			UNIQUE(session_id, turn_index)
		)`,
		`CREATE INDEX idx_turns_session ON turns(session_id)`,
		`CREATE TABLE checkpoints (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			checkpoint_number INTEGER NOT NULL,
			title TEXT,
			overview TEXT,
			history TEXT,
			work_done TEXT,
			technical_details TEXT,
			important_files TEXT,
			next_steps TEXT,
			created_at TEXT DEFAULT (datetime('now')),
			UNIQUE(session_id, checkpoint_number)
		)`,
		`CREATE INDEX idx_checkpoints_session ON checkpoints(session_id)`,
		`CREATE TABLE session_files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			file_path TEXT NOT NULL,
			tool_name TEXT,
			turn_index INTEGER,
			first_seen_at TEXT DEFAULT (datetime('now')),
			UNIQUE(session_id, file_path)
		)`,
		`CREATE INDEX idx_session_files_path ON session_files(file_path)`,
		`CREATE TABLE session_refs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id),
			ref_type TEXT NOT NULL,
			ref_value TEXT NOT NULL,
			turn_index INTEGER,
			created_at TEXT DEFAULT (datetime('now')),
			UNIQUE(session_id, ref_type, ref_value)
		)`,
		`CREATE INDEX idx_session_refs_type_value ON session_refs(ref_type, ref_value)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}

	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO schema_version (version) VALUES (5)`, nil},
		{`INSERT INTO sessions (id, cwd, repository, host_type, branch, summary, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{"a1", wsA, "swelljoe/projA", "github", "main", "A1", "2026-07-01T00:00:00Z", "2026-07-02T00:00:00Z"}},
		{`INSERT INTO sessions (id, cwd, repository, host_type, branch, summary, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{"a2", wsA, "swelljoe/projA", "github", "main", "A2", "2026-07-01T01:00:00Z", "2026-07-01T02:00:00Z"}},
		{`INSERT INTO sessions (id, cwd, repository, host_type, branch, summary, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{"b1", wsB, "swelljoe/projB", "github", "main", "B1", "2026-07-01T03:00:00Z", "2026-07-01T04:00:00Z"}},
		{`INSERT INTO turns (id, session_id, turn_index, user_message, assistant_response, timestamp) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{1, "a1", 0, "A prompt", "A reply", "2026-07-02T00:00:01Z"}},
		{`INSERT INTO turns (id, session_id, turn_index, user_message, assistant_response, timestamp) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{2, "a2", 0, "A2 prompt", "A2 reply", "2026-07-01T01:00:01Z"}},
		{`INSERT INTO turns (id, session_id, turn_index, user_message, assistant_response, timestamp) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{3, "b1", 0, "B prompt", "B reply", "2026-07-01T03:00:01Z"}},
		{`INSERT INTO checkpoints (id, session_id, checkpoint_number, title, created_at) VALUES (?, ?, ?, ?, ?)`,
			[]any{1, "a1", 1, "cp-a1", "2026-07-02T00:00:02Z"}},
		{`INSERT INTO checkpoints (id, session_id, checkpoint_number, title, created_at) VALUES (?, ?, ?, ?, ?)`,
			[]any{2, "b1", 1, "cp-b1", "2026-07-01T03:00:02Z"}},
		{`INSERT INTO session_files (id, session_id, file_path, tool_name, turn_index, first_seen_at) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{1, "a1", "/tmp/a.txt", "view", 0, "2026-07-02T00:00:03Z"}},
		{`INSERT INTO session_files (id, session_id, file_path, tool_name, turn_index, first_seen_at) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{2, "b1", "/tmp/b.txt", "view", 0, "2026-07-01T03:00:03Z"}},
		{`INSERT INTO session_refs (id, session_id, ref_type, ref_value, turn_index, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{1, "a1", "pr", "1", 0, "2026-07-02T00:00:04Z"}},
		{`INSERT INTO session_refs (id, session_id, ref_type, ref_value, turn_index, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{2, "b1", "pr", "2", 0, "2026-07-01T03:00:04Z"}},
	} {
		if _, err := db.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatalf("seed db: %v", err)
		}
	}

	writeFile(t, filepath.Join(root, copilotSessionState, "a1", "workspace.yaml"), "cwd: "+wsA+"\n")
	writeFile(t, filepath.Join(root, copilotSessionState, "a1", "events.jsonl"), `{"session":"a1"}`+"\n")
	writeFile(t, filepath.Join(root, copilotSessionState, "a1", "inuse.123.lock"), "")
	writeFile(t, filepath.Join(root, copilotSessionState, "a2", "workspace.yaml"), "cwd: "+wsA+"\n")
	writeFile(t, filepath.Join(root, copilotSessionState, "b1", "workspace.yaml"), "cwd: "+wsB+"\n")

	return home
}

func buildCopiedCopilotConfig(t *testing.T, hostHome string) string {
	t.Helper()
	src := filepath.Join(hostHome, ".copilot")
	dst := filepath.Join(t.TempDir(), ".copilot")
	if err := CopyDirExcept(src, dst, copilotSkipCopy); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestPrepareCopilotStoreScoping(t *testing.T) {
	wsA, wsB := "/home/joe/src/projA", "/home/joe/src/projB"
	home := buildCopilotHostStore(t, wsA, wsB)
	configSrc := buildCopiedCopilotConfig(t, home)

	store, err := prepareCopilotStore(home, wsA, configSrc)
	if err != nil {
		t.Fatal(err)
	}

	mustExist(t, store, "settings.json")
	mustExist(t, store, filepath.Join(copilotSessionState, "a1", "workspace.yaml"))
	mustExist(t, store, filepath.Join(copilotSessionState, "a2", "workspace.yaml"))
	mustAbsent(t, store, filepath.Join(copilotSessionState, "b1", "workspace.yaml"))
	mustAbsent(t, store, filepath.Join(copilotSessionState, "a1", "inuse.123.lock"))

	db, err := openSQLite(filepath.Join(store, copilotSessionStoreDB))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT id, cwd FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var id, cwd string
		if err := rows.Scan(&id, &cwd); err != nil {
			t.Fatal(err)
		}
		got = append(got, id+"@"+cwd)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a1@"+wsA || got[1] != "a2@"+wsA {
		t.Fatalf("scoped sessions = %v, want only workspace A", got)
	}

	var turns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 2 {
		t.Fatalf("scoped turns count = %d, want 2", turns)
	}

	var refs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_refs`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 1 {
		t.Fatalf("scoped refs count = %d, want 1", refs)
	}
}

func TestPrepareCopilotStoreSeedsOnce(t *testing.T) {
	wsA, wsB := "/home/joe/src/projA", "/home/joe/src/projB"
	home := buildCopilotHostStore(t, wsA, wsB)
	configSrc := buildCopiedCopilotConfig(t, home)

	store, err := prepareCopilotStore(home, wsA, configSrc)
	if err != nil {
		t.Fatal(err)
	}

	edited := filepath.Join(store, copilotSessionState, "a1", "workspace.yaml")
	if err := os.WriteFile(edited, []byte("edited-in-flar\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hostDB, err := openSQLite(filepath.Join(home, ".copilot", copilotSessionStoreDB))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hostDB.Exec(
		`INSERT INTO sessions (id, cwd, repository, host_type, branch, summary, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"a3", wsA, "swelljoe/projA", "github", "main", "host-only", "2026-07-02T01:00:00Z", "2026-07-02T01:00:01Z",
	); err != nil {
		t.Fatal(err)
	}
	hostDB.Close()
	writeFile(t, filepath.Join(home, ".copilot", copilotSessionState, "a3", "workspace.yaml"), "cwd: "+wsA+"\n")

	if _, err := prepareCopilotStore(home, wsA, configSrc); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "edited-in-flar\n" {
		t.Fatalf("workspace state was clobbered on second prepare: %q", got)
	}
	mustAbsent(t, store, filepath.Join(copilotSessionState, "a3", "workspace.yaml"))
}

func TestCopyDirExceptSkipsCopilotSessionData(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")
	writeFile(t, filepath.Join(src, "settings.json"), "{}")
	writeFile(t, filepath.Join(src, copilotSessionState, "s1", "workspace.yaml"), "cwd: /tmp\n")
	writeFile(t, filepath.Join(src, copilotSessionStoreDB), "db")
	writeFile(t, filepath.Join(src, copilotStoreRel, "slug", copilotSessionStoreDB), "scoped-db")

	if err := CopyDirExcept(src, dst, copilotSkipCopy); err != nil {
		t.Fatal(err)
	}

	mustExist(t, dst, "settings.json")
	mustAbsent(t, dst, copilotSessionState)
	mustAbsent(t, dst, copilotSessionStoreDB)
	mustAbsent(t, dst, copilotStoreRel)
}
