package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// mimoSkipCopy lists paths under the mimo data dir (~/.local/share/mimocode/)
// that flar does NOT copy into the sandbox config:
//   - mimocode.db*: the global SQLite database mixing every project's sessions.
//     The current project's sessions are supplied at run time by a per-project
//     shadow store (see prepareMimoStore); copying the full DB here would leak
//     every other project's conversation history into the sandbox.
//   - memory/: per-project and per-session memory files. The current project's
//     memory is live-bound from the host; copying it here would expose every
//     other project's memory.
//   - log/: global logs mixing all projects.
//   - snapshot/: global checkpoint snapshots.
var mimoSkipCopy = map[string]bool{
	"mimocode.db":     true,
	"mimocode.db-wal": true,
	"mimocode.db-shm": true,
	"memory":          true,
	"log":             true,
	"snapshot":        true,
}

// mimoConfigSkipCopy lists paths under ~/.config/mimocode/ that flar does NOT
// copy. node_modules/ and package*.json are installation scaffolding, not user
// configuration.
var mimoConfigSkipCopy = map[string]bool{
	"node_modules":    true,
	"package.json":    true,
	"package-lock.json": true,
}

// mimoDataDir returns the host-side mimo data directory, respecting
// XDG_DATA_HOME.
func mimoDataDir(home string) string {
	if dataHome := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dataHome) {
		return filepath.Join(dataHome, "mimocode")
	}
	return filepath.Join(home, ".local", "share", "mimocode")
}

// mimoConfigDir returns the host-side mimo config directory, respecting
// XDG_CONFIG_HOME.
func mimoConfigDir(home string) string {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(configHome) {
		return filepath.Join(configHome, "mimocode")
	}
	return filepath.Join(home, ".config", "mimocode")
}

// prepareMimoStore returns a persistent, project-only mimo data store. It is
// seeded once from the trusted host database and never merged back: after mimo
// runs in the sandbox every file in this directory must be treated as
// attacker-controlled.
//
// mimo keeps every session in a single global SQLite database (mimocode.db)
// that mixes all projects, so flar forks it into a per-project shadow home
// under $XDG_STATE_HOME/flar/mimo/<slug>/. The shadow home is bind-mounted as
// ~/.local/share/mimocode inside the sandbox, so sessions created in flar
// persist and can be resumed with `mimo --continue`, while other projects'
// sessions stay invisible.
func prepareMimoStore(hostHome, absProjectDir string) (string, error) {
	hostData := mimoDataDir(hostHome)
	store := filepath.Join(flarStateDir(hostHome), "mimo", claudeProjectSlug(absProjectDir))

	if err := os.MkdirAll(store, 0o700); err != nil {
		return "", err
	}

	marker := filepath.Join(store, ".seeded")
	if fileExists(marker) {
		return store, nil
	}

	if err := seedMimoStore(hostData, store, absProjectDir); err != nil {
		return "", err
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return "", err
	}
	return store, nil
}

// seedMimoStore copies this project's existing host sessions into the scoped
// shadow database, and copies the project's memory directory.
func seedMimoStore(hostData, store, absProjectDir string) error {
	hostDB := filepath.Join(hostData, "mimocode.db")
	if !fileExists(hostDB) {
		// No database on the host; create an empty one so mimo can initialize.
		return nil
	}

	if err := filterMimoDB(hostDB, filepath.Join(store, "mimocode.db"), absProjectDir); err != nil {
		return err
	}

	// Copy this project's memory directory into the store. mimo stores
	// project memory at memory/projects/<uuid>/; the UUID comes from the
	// project table. Session memory (memory/sessions/<id>/) is copied for
	// sessions belonging to this project.
	if err := seedMimoMemory(hostData, store, absProjectDir); err != nil {
		// Best-effort: memory is not required for basic operation.
		_ = err
	}

	return nil
}

// filterMimoDB creates a shadow copy of the mimo database containing only the
// sessions belonging to absProjectDir. It works by:
//  1. VACUUM INTO to snapshot the host database (consistent including WAL).
//  2. Deleting all projects except the target, which cascades to sessions,
//     messages, parts, tasks, actors, and related tables.
//  3. Cleaning up global tables (accounts, FTS indices, event sequences).
//  4. VACUUM to reclaim space.
func filterMimoDB(src, dst, absProjectDir string) error {
	_ = os.Remove(dst)
	_ = os.Remove(dst + "-wal")
	_ = os.Remove(dst + "-shm")

	srcDB, err := openSQLite(src)
	if err != nil {
		return err
	}
	defer srcDB.Close()

	// VACUUM INTO gives a consistent snapshot including committed WAL pages.
	if _, err := srcDB.Exec(`VACUUM INTO ?`, dst); err != nil {
		return err
	}
	srcDB.Close()

	db, err := openSQLite(dst)
	if err != nil {
		return err
	}
	defer db.Close()

	// Enable foreign key enforcement so ON DELETE CASCADE actually fires.
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return err
	}

	// Find the project ID for this worktree.
	var projectID string
	err = db.QueryRow(`SELECT id FROM project WHERE worktree = ?`, absProjectDir).Scan(&projectID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	// Delete all projects except the target. If the target doesn't exist
	// (ErrNoRows), every project is deleted, leaving an empty shadow DB.
	if projectID != "" {
		if _, err := db.Exec(`DELETE FROM project WHERE id <> ?`, projectID); err != nil {
			return err
		}
	} else {
		if _, err := db.Exec(`DELETE FROM project`); err != nil {
			return err
		}
	}

	// Clean up tables that don't cascade from project deletion.
	// Orphaned sessions (shouldn't exist, but defense in depth).
	if projectID != "" {
		if _, err := db.Exec(`DELETE FROM session WHERE project_id <> ?`, projectID); err != nil {
			return err
		}
	} else {
		if _, err := db.Exec(`DELETE FROM session`); err != nil {
			return err
		}
	}

	// Import tables keyed by session_id. These may not exist in older
	// database versions, so ignore errors.
	for _, table := range []string{"claude_import", "external_import"} {
		_, _ = db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE session_id NOT IN (SELECT id FROM session)`, table))
	}

	// Global tables unrelated to any project's resume.
	for _, table := range []string{
		"account",
		"account_state",
		"control_account",
	} {
		_, _ = db.Exec(`DELETE FROM ` + table)
	}

	// FTS index tables: clear them. The triggers on the content tables
	// (history_fts, memory_fts) already handle deletions from those tables,
	// but the index virtual tables may accumulate stale entries.
	for _, table := range []string{
		"history_fts_idx",
		"memory_fts_idx",
	} {
		_, _ = db.Exec(`DELETE FROM ` + table)
	}

	// Checkpoint WAL to reclaim space.
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)

	// Final VACUUM to compact the shadow database.
	db.Close()
	vdb, err := openSQLite(dst)
	if err != nil {
		return err
	}
	defer vdb.Close()
	_, _ = vdb.Exec(`VACUUM`)
	return nil
}

// mimoProjectMemoryDir returns the path to this project's memory directory
// inside the shadow store, or "" if it doesn't exist. Used to live-bind the
// host's copy so memory written in the sandbox persists.
func mimoProjectMemoryDir(store, absProjectDir string) string {
	db, err := openSQLite(filepath.Join(store, "mimocode.db"))
	if err != nil {
		return ""
	}
	defer db.Close()
	var projectID string
	if err := db.QueryRow(`SELECT id FROM project WHERE worktree = ?`, absProjectDir).Scan(&projectID); err != nil {
		return ""
	}
	dir := filepath.Join(store, "memory", "projects", projectID)
	if dirExists(dir) {
		return dir
	}
	return ""
}

// seedMimoMemory copies the project's memory directory into the shadow store.
// mimo organizes memory as:
//   - memory/projects/<uuid>/  — per-project memory (MEMORY.md, etc.)
//   - memory/sessions/<id>/    — per-session memory (checkpoint.md, notes.md, etc.)
//
// The project UUID is looked up from the shadow database. Session directories
// are copied for sessions belonging to this project.
func seedMimoMemory(hostData, store, absProjectDir string) error {
	shadowDB := filepath.Join(store, "mimocode.db")
	if !fileExists(shadowDB) {
		return nil
	}

	db, err := openSQLite(shadowDB)
	if err != nil {
		return err
	}
	defer db.Close()

	// Find project ID.
	var projectID string
	if err := db.QueryRow(`SELECT id FROM project WHERE worktree = ?`, absProjectDir).Scan(&projectID); err != nil {
		return nil // no project, no memory to copy
	}

	// Copy project memory.
	hostProjMem := filepath.Join(hostData, "memory", "projects", projectID)
	if dirExists(hostProjMem) {
		dstProjMem := filepath.Join(store, "memory", "projects", projectID)
		if err := CopyDir(hostProjMem, dstProjMem); err != nil {
			return err
		}
	}

	// Copy session memory for this project's sessions.
	rows, err := db.Query(`SELECT id FROM session WHERE project_id = ?`, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return err
		}
		hostSessMem := filepath.Join(hostData, "memory", "sessions", sessionID)
		if dirExists(hostSessMem) {
			dstSessMem := filepath.Join(store, "memory", "sessions", sessionID)
			if err := CopyDir(hostSessMem, dstSessMem); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}
