package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMimoConfigIsolation verifies that only non-database, non-memory config
// files are copied from the mimo data directory, and that the global database,
// memory, logs, and snapshots are left behind.
func TestMimoConfigIsolation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "mimocode-data")

	// Files that must be copied.
	writeFile(t, filepath.Join(src, "auth.json"), `{"provider":{"type":"api","key":"secret"}}`)
	writeFile(t, filepath.Join(src, "installation_id"), "uuid-1234")
	writeFile(t, filepath.Join(src, "mimo-key-name"), "key-name")
	writeFile(t, filepath.Join(src, "trusted-workspaces.json"), `[]`)
	writeFile(t, filepath.Join(src, "builtin_skills", "skill.md"), "skill content")
	writeFile(t, filepath.Join(src, "compose", "workflow.js"), "workflow")

	// Cross-project data that must NOT leak.
	writeFile(t, filepath.Join(src, "mimocode.db"), "fake-db")
	writeFile(t, filepath.Join(src, "mimocode.db-wal"), "fake-wal")
	writeFile(t, filepath.Join(src, "mimocode.db-shm"), "fake-shm")
	writeFile(t, filepath.Join(src, "memory", "projects", "uuid-1", "MEMORY.md"), "project memory")
	writeFile(t, filepath.Join(src, "memory", "sessions", "ses-1", "checkpoint.md"), "session checkpoint")
	writeFile(t, filepath.Join(src, "log", "app.log"), "log data")
	writeFile(t, filepath.Join(src, "snapshot", "snap.json"), "snapshot")

	if err := CopyDirExcept(src, dst, mimoSkipCopy); err != nil {
		t.Fatalf("CopyDirExcept: %v", err)
	}

	// Config files must exist.
	mustExist(t, dst, "auth.json")
	mustExist(t, dst, "installation_id")
	mustExist(t, dst, "mimo-key-name")
	mustExist(t, dst, "trusted-workspaces.json")
	mustExist(t, dst, filepath.Join("builtin_skills", "skill.md"))
	mustExist(t, dst, filepath.Join("compose", "workflow.js"))

	// Database, memory, logs, snapshots must NOT exist.
	mustAbsent(t, dst, "mimocode.db")
	mustAbsent(t, dst, "mimocode.db-wal")
	mustAbsent(t, dst, "mimocode.db-shm")
	mustAbsent(t, dst, "memory")
	mustAbsent(t, dst, "log")
	mustAbsent(t, dst, "snapshot")
}

// TestMimoConfigDirIsolation verifies that only the user config file is copied
// from ~/.config/mimocode/, and that node_modules and package files are skipped.
func TestMimoConfigDirIsolation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "mimocode-config")

	writeFile(t, filepath.Join(src, "mimocode.jsonc"), `{"model":"mimo-v2.5-pro"}`)
	writeFile(t, filepath.Join(src, "node_modules", "pkg", "index.js"), "module")
	writeFile(t, filepath.Join(src, "package.json"), "{}")
	writeFile(t, filepath.Join(src, "package-lock.json"), "{}")

	if err := CopyDirExcept(src, dst, mimoConfigSkipCopy); err != nil {
		t.Fatalf("CopyDirExcept: %v", err)
	}

	mustExist(t, dst, "mimocode.jsonc")
	mustAbsent(t, dst, "node_modules")
	mustAbsent(t, dst, "package.json")
	mustAbsent(t, dst, "package-lock.json")
}

// initMimoTestDB creates a minimal mimo database schema for testing.
func initMimoTestDB(t *testing.T, path string) {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE project (
			id TEXT PRIMARY KEY,
			worktree TEXT NOT NULL,
			vcs TEXT,
			name TEXT,
			icon_url TEXT,
			icon_color TEXT,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			time_initialized INTEGER,
			sandboxes TEXT NOT NULL,
			commands TEXT
		);
		CREATE TABLE session (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
			parent_id TEXT,
			slug TEXT NOT NULL,
			directory TEXT NOT NULL,
			title TEXT NOT NULL,
			version TEXT NOT NULL,
			share_url TEXT,
			summary_additions INTEGER,
			summary_deletions INTEGER,
			summary_files INTEGER,
			summary_diffs TEXT,
			revert TEXT,
			permission TEXT,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			time_compacting INTEGER,
			time_archived INTEGER,
			workspace_id TEXT,
			context_from TEXT,
			context_watermark TEXT,
			last_checkpoint_message_id TEXT
		);
		CREATE INDEX session_project_idx ON session(project_id);
		CREATE TABLE message (
			id TEXT PRIMARY KEY NOT NULL,
			session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
			agent_id TEXT NOT NULL DEFAULT 'main',
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		);
		CREATE TABLE part (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL,
			FOREIGN KEY (message_id) REFERENCES message(id) ON DELETE CASCADE
		);
		CREATE TABLE workspace (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			name TEXT DEFAULT '' NOT NULL,
			branch TEXT,
			directory TEXT,
			extra TEXT,
			project_id TEXT NOT NULL,
			FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE
		);
		CREATE TABLE permission (
			project_id TEXT PRIMARY KEY,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL,
			FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE
		);
		CREATE TABLE task (
			id TEXT NOT NULL,
			session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
			parent_task_id TEXT,
			status TEXT NOT NULL,
			summary TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_event_at INTEGER NOT NULL,
			ended_at INTEGER,
			cleanup_after INTEGER,
			owner TEXT,
			PRIMARY KEY (session_id, id)
		);
		CREATE TABLE actor_registry (
			session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
			actor_id TEXT NOT NULL,
			mode TEXT NOT NULL,
			parent_actor_id TEXT,
			status TEXT NOT NULL,
			agent TEXT NOT NULL,
			description TEXT NOT NULL,
			context_mode TEXT NOT NULL,
			context_watermark TEXT,
			background INTEGER NOT NULL,
			tools TEXT,
			last_turn_time INTEGER NOT NULL,
			turn_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			time_completed INTEGER,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			last_outcome TEXT,
			lifecycle TEXT NOT NULL DEFAULT 'ephemeral',
			instance_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (session_id, actor_id)
		);
		CREATE TABLE account (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL,
			url TEXT NOT NULL,
			access_token TEXT NOT NULL,
			refresh_token TEXT NOT NULL,
			token_expiry INTEGER,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL
		);
		CREATE TABLE account_state (
			id INTEGER PRIMARY KEY NOT NULL,
			active_account_id TEXT,
			active_org_id TEXT
		);
		CREATE TABLE control_account (
			email TEXT NOT NULL,
			url TEXT NOT NULL,
			access_token TEXT NOT NULL,
			refresh_token TEXT NOT NULL,
			token_expiry INTEGER,
			active INTEGER NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			PRIMARY KEY (email, url)
		);
		CREATE TABLE history_fts (
			part_id TEXT PRIMARY KEY NOT NULL,
			session_id TEXT NOT NULL,
			message_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			tool_name TEXT,
			body TEXT NOT NULL,
			time_created INTEGER NOT NULL
		);
		CREATE TABLE memory_fts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT NOT NULL UNIQUE,
			scope TEXT NOT NULL,
			scope_id TEXT DEFAULT '' NOT NULL,
			type TEXT NOT NULL,
			body TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			last_indexed_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
}

// TestFilterMimoDBSeedsOnlyCurrentProject verifies that the shadow database
// contains only the sessions, messages, and related data for the target project.
func TestFilterMimoDBSeedsOnlyCurrentProject(t *testing.T) {
	src := filepath.Join(t.TempDir(), "host.db")
	initMimoTestDB(t, src)

	db, err := openSQLite(src)
	if err != nil {
		t.Fatal(err)
	}

	// Insert two projects.
	_, err = db.Exec(`
		INSERT INTO project (id, worktree, time_created, time_updated, sandboxes)
		VALUES ('proj-a', '/home/user/project-a', 0, 0, '[]'),
		       ('proj-b', '/home/user/project-b', 0, 0, '[]');
		INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated)
		VALUES ('sess-a1', 'proj-a', 's1', '/home/user/project-a', 'Session A1', '1', 0, 0),
		       ('sess-a2', 'proj-a', 's2', '/home/user/project-a', 'Session A2', '1', 0, 0),
		       ('sess-b1', 'proj-b', 's1', '/home/user/project-b', 'Session B1', '1', 0, 0);
		INSERT INTO message (id, session_id, time_created, time_updated, data)
		VALUES ('msg-a1', 'sess-a1', 0, 0, '{"role":"user"}'),
		       ('msg-b1', 'sess-b1', 0, 0, '{"role":"user"}');
		INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
		VALUES ('part-a1', 'msg-a1', 'sess-a1', 0, 0, '{"type":"text"}'),
		       ('part-b1', 'msg-b1', 'sess-b1', 0, 0, '{"type":"text"}');
		INSERT INTO workspace (id, type, name, project_id)
		VALUES ('ws-a', 'main', 'A', 'proj-a'),
		       ('ws-b', 'main', 'B', 'proj-b');
		INSERT INTO permission (project_id, time_created, time_updated, data)
		VALUES ('proj-a', 0, 0, '{}'),
		       ('proj-b', 0, 0, '{}');
		INSERT INTO task (id, session_id, status, summary, created_at, last_event_at)
		VALUES ('t1', 'sess-a1', 'done', 'task a1', 0, 0),
		       ('t2', 'sess-b1', 'done', 'task b1', 0, 0);
		INSERT INTO account (id, email, url, access_token, refresh_token, time_created, time_updated)
		VALUES ('acc-1', 'user@example.com', 'https://api.example.com', 'tok', 'ref', 0, 0);
		INSERT INTO account_state (id, active_account_id) VALUES (1, 'acc-1');
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	dst := filepath.Join(t.TempDir(), "shadow.db")
	if err := filterMimoDB(src, dst, "/home/user/project-a"); err != nil {
		t.Fatalf("filterMimoDB: %v", err)
	}

	shadow, err := openSQLite(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer shadow.Close()

	// Project A should remain, B should be gone.
	var count int
	if err := shadow.QueryRow(`SELECT count(*) FROM project`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("project count = %d, want 1", count)
	}

	// Sessions for A should remain, B's should be gone.
	if err := shadow.QueryRow(`SELECT count(*) FROM session`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("session count = %d, want 2", count)
	}

	// Messages: only A's.
	if err := shadow.QueryRow(`SELECT count(*) FROM message`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("message count = %d, want 1", count)
	}

	// Parts: only A's.
	if err := shadow.QueryRow(`SELECT count(*) FROM part`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("part count = %d, want 1", count)
	}

	// Tasks: only A's.
	if err := shadow.QueryRow(`SELECT count(*) FROM task`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("task count = %d, want 1", count)
	}

	// Accounts should be cleared.
	if err := shadow.QueryRow(`SELECT count(*) FROM account`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("account count = %d, want 0", count)
	}

	// Workspaces: only A's.
	if err := shadow.QueryRow(`SELECT count(*) FROM workspace`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("workspace count = %d, want 1", count)
	}
}

// TestFilterMimoDBFreshHost verifies filtering against a database with no
// matching project: the shadow DB is empty but valid.
func TestFilterMimoDBFreshHost(t *testing.T) {
	src := filepath.Join(t.TempDir(), "host.db")
	initMimoTestDB(t, src)

	db, err := openSQLite(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO project (id, worktree, time_created, time_updated, sandboxes)
		VALUES ('proj-x', '/home/user/other', 0, 0, '[]');
	`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "shadow.db")
	if err := filterMimoDB(src, dst, "/home/user/project-a"); err != nil {
		t.Fatalf("filterMimoDB: %v", err)
	}

	// The shadow DB should exist and be openable.
	shadow, err := openSQLite(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer shadow.Close()

	var count int
	if err := shadow.QueryRow(`SELECT count(*) FROM project`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("project count = %d, want 0 (no matching project)", count)
	}
}

// TestPrepareMimoStoreSeedsOnce verifies the fork semantics: after the one-time
// seed, later host-side sessions are not pulled in and store contents are not
// clobbered.
func TestPrepareMimoStoreSeedsOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")

	projA := "/home/joe/src/flar"
	projB := "/home/joe/src/other"

	hostHome := t.TempDir()
	hostData := filepath.Join(hostHome, ".local", "share", "mimocode")
	if err := os.MkdirAll(hostData, 0o700); err != nil {
		t.Fatal(err)
	}

	// Create a host database with sessions for both projects.
	hostDB := filepath.Join(hostData, "mimocode.db")
	initMimoTestDB(t, hostDB)
	db, err := openSQLite(hostDB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO project (id, worktree, time_created, time_updated, sandboxes)
		VALUES ('proj-a', ?, 0, 0, '[]'),
		       ('proj-b', ?, 0, 0, '[]');
		INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated)
		VALUES ('sess-a1', 'proj-a', 's1', ?, 'A1', '1', 0, 0),
		       ('sess-b1', 'proj-b', 's1', ?, 'B1', '1', 0, 0);
	`, projA, projB, projA, projB)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	store, err := prepareMimoStore(hostHome, projA)
	if err != nil {
		t.Fatalf("prepareMimoStore: %v", err)
	}

	mustExist(t, store, ".seeded")
	mustExist(t, store, "mimocode.db")

	// Verify shadow DB has only project A.
	shadow, err := openSQLite(filepath.Join(store, "mimocode.db"))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := shadow.QueryRow(`SELECT count(*) FROM session`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	shadow.Close()
	if count != 1 {
		t.Errorf("session count = %d, want 1", count)
	}

	// Simulate a sandbox-created session by modifying the store.
	writeFile(t, filepath.Join(store, "sandbox-file.txt"), "sandbox data")

	// Add a new host-side session — should NOT be pulled in on second prepare.
	db, err = openSQLite(hostDB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated)
		VALUES ('sess-a2', 'proj-a', 's2', ?, 'A2', '1', 0, 0);
	`, projA)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	store2, err := prepareMimoStore(hostHome, projA)
	if err != nil || store2 != store {
		t.Fatalf("second prepare = %q, %v; want %q", store2, err, store)
	}

	// Sandbox file should still exist.
	mustExist(t, store, "sandbox-file.txt")

	// The new session should NOT appear in the shadow DB.
	shadow, err = openSQLite(filepath.Join(store, "mimocode.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := shadow.QueryRow(`SELECT count(*) FROM session WHERE id = 'sess-a2'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	shadow.Close()
	if count != 0 {
		t.Errorf("new host session leaked into shadow store after seeding")
	}
}

// TestMimoDataDir verifies the data directory resolution.
func TestMimoDataDir(t *testing.T) {
	home := "/home/test"

	t.Setenv("XDG_DATA_HOME", "")
	if got, want := mimoDataDir(home), "/home/test/.local/share/mimocode"; got != want {
		t.Errorf("mimoDataDir (default) = %q, want %q", got, want)
	}

	t.Setenv("XDG_DATA_HOME", "/var/lib/data")
	if got, want := mimoDataDir(home), "/var/lib/data/mimocode"; got != want {
		t.Errorf("mimoDataDir (XDG) = %q, want %q", got, want)
	}

	t.Setenv("XDG_DATA_HOME", "relative")
	if got, want := mimoDataDir(home), "/home/test/.local/share/mimocode"; got != want {
		t.Errorf("mimoDataDir (relative XDG) = %q, want %q", got, want)
	}
}

// TestMimoConfigDir verifies the config directory resolution.
func TestMimoConfigDir(t *testing.T) {
	home := "/home/test"

	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := mimoConfigDir(home), "/home/test/.config/mimocode"; got != want {
		t.Errorf("mimoConfigDir (default) = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "/etc/config")
	if got, want := mimoConfigDir(home), "/etc/config/mimocode"; got != want {
		t.Errorf("mimoConfigDir (XDG) = %q, want %q", got, want)
	}
}

// TestFilterMimoDBNoDatabase verifies that filterMimoDB handles a missing
// source database gracefully.
func TestFilterMimoDBNoDatabase(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "shadow.db")
	err := filterMimoDB("/nonexistent/db", dst, "/home/user/project")
	if err == nil {
		t.Error("expected error for missing source database, got nil")
	}
}

// TestPrepareMimoStoreNoDatabase verifies that prepareMimoStore handles a host
// with no mimo database at all.
func TestPrepareMimoStoreNoDatabase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	hostHome := t.TempDir()
	// Don't create any mimo data directory.

	store, err := prepareMimoStore(hostHome, "/home/joe/src/flar")
	if err != nil {
		t.Fatalf("prepareMimoStore: %v", err)
	}
	mustExist(t, store, ".seeded")
	mustAbsent(t, store, "mimocode.db")
}

// TestAutoDetectAgentMimo verifies that mimo is auto-detected when ~/.mimocode
// exists or XIAOMI_API_KEY is set.
func TestAutoDetectAgentMimo(t *testing.T) {
	unsetAgentEnvs(t)

	home := t.TempDir()
	t.Setenv("HOME", home)

	// Detection via ~/.mimocode directory.
	if err := os.MkdirAll(filepath.Join(home, ".mimocode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := autoDetectAgent(); got != AgentMimo {
		t.Errorf("autoDetectAgent() = %q, want %q (with ~/.mimocode)", got, AgentMimo)
	}

	// Remove directory, test via env var.
	os.RemoveAll(filepath.Join(home, ".mimocode"))
	t.Setenv("XIAOMI_API_KEY", "test-key")
	if got := autoDetectAgent(); got != AgentMimo {
		t.Errorf("autoDetectAgent() = %q, want %q (with XIAOMI_API_KEY)", got, AgentMimo)
	}
}

// TestMimoSessionCount verifies that the filter correctly counts sessions
// after filtering.
func TestMimoSessionCount(t *testing.T) {
	src := filepath.Join(t.TempDir(), "host.db")
	initMimoTestDB(t, src)

	projDir := "/home/user/project-a"

	db, err := openSQLite(src)
	if err != nil {
		t.Fatal(err)
	}

	// Insert project and 3 sessions.
	_, err = db.Exec(`
		INSERT INTO project (id, worktree, time_created, time_updated, sandboxes)
		VALUES ('proj-a', ?, 0, 0, '[]');
	`, projDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, err = db.Exec(`
			INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated)
			VALUES (?, 'proj-a', ?, ?, ?, '1', 0, 0);
		`, "sess-"+string(rune('a'+i)), "s"+string(rune('a'+i)), projDir, "Session "+string(rune('A'+i)))
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	dst := filepath.Join(t.TempDir(), "shadow.db")
	if err := filterMimoDB(src, dst, projDir); err != nil {
		t.Fatalf("filterMimoDB: %v", err)
	}

	shadow, err := openSQLite(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer shadow.Close()

	var count int
	if err := shadow.QueryRow(`SELECT count(*) FROM session`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("session count = %d, want 3", count)
	}

	// Verify all sessions belong to this project.
	if err := shadow.QueryRow(`SELECT count(*) FROM session WHERE project_id <> 'proj-a'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("foreign session count = %d, want 0", count)
	}
}

// Helper to open SQLite for mimo tests (reuses the copilot helper).
func openMimoSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestMimoFilterPreservesSessionData verifies that message and part data is
// preserved in the shadow database for the target project's sessions.
func TestMimoFilterPreservesSessionData(t *testing.T) {
	src := filepath.Join(t.TempDir(), "host.db")
	initMimoTestDB(t, src)

	projDir := "/home/user/project-a"

	db := openMimoSQLite(t, src)
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO project (id, worktree, time_created, time_updated, sandboxes)
		VALUES ('proj-a', ?, 0, 0, '[]');
		INSERT INTO session (id, project_id, slug, directory, title, version, time_created, time_updated)
		VALUES ('sess-1', 'proj-a', 's1', ?, 'Test', '1', 0, 0);
		INSERT INTO message (id, session_id, time_created, time_updated, data)
		VALUES ('msg-1', 'sess-1', 0, 0, '{"role":"user","content":"hello"}'),
		       ('msg-2', 'sess-1', 0, 0, '{"role":"assistant","content":"hi"}');
		INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
		VALUES ('part-1', 'msg-1', 'sess-1', 0, 0, '{"type":"text","text":"hello"}'),
		       ('part-2', 'msg-2', 'sess-1', 0, 0, '{"type":"text","text":"hi"}');
	`, projDir, projDir)
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "shadow.db")
	if err := filterMimoDB(src, dst, projDir); err != nil {
		t.Fatalf("filterMimoDB: %v", err)
	}

	shadow := openMimoSQLite(t, dst)
	defer shadow.Close()

	// Verify message data is preserved.
	var data string
	if err := shadow.QueryRow(`SELECT data FROM message WHERE id = 'msg-1'`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, "hello") {
		t.Errorf("message data not preserved: %s", data)
	}

	// Verify part data is preserved.
	if err := shadow.QueryRow(`SELECT data FROM part WHERE id = 'part-2'`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, "hi") {
		t.Errorf("part data not preserved: %s", data)
	}
}
