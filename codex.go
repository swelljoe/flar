package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const codexStateDB = "state_5.sqlite"

var codexSkipCopy = map[string]bool{
	"sessions":              true,
	"history.jsonl":         true,
	"shell_snapshots":       true,
	"logs_2.sqlite":         true,
	"logs_2.sqlite-wal":     true,
	"logs_2.sqlite-shm":     true,
	"goals_1.sqlite":        true,
	"goals_1.sqlite-wal":    true,
	"goals_1.sqlite-shm":    true,
	"memories_1.sqlite":     true,
	"memories_1.sqlite-wal": true,
	"memories_1.sqlite-shm": true,
	"state_5.sqlite":        true,
	"state_5.sqlite-wal":    true,
	"state_5.sqlite-shm":    true,
	".flar":                 true, // legacy development location; never expose it
}

// prepareCodexStore returns a persistent, project-only Codex home. It is seeded
// once from the trusted host store and never merged back: after Codex runs in the
// sandbox every file in this directory must be treated as attacker-controlled.
func prepareCodexStore(hostHome, cwd, configSrc string) (string, error) {
	hostCodex := filepath.Join(hostHome, ".codex")
	store := filepath.Join(flarStateDir(hostHome), "codex", claudeProjectSlug(cwd))
	marker := filepath.Join(store, ".seeded")
	if fileExists(marker) {
		return store, nil
	}
	if err := CopyDirExcept(configSrc, store, codexSkipCopy); err != nil {
		return "", err
	}
	if err := seedCodexStore(hostCodex, store, cwd); err != nil {
		return "", err
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return "", err
	}
	return store, nil
}

// flarStateDir follows the XDG Base Directory specification. XDG_STATE_HOME
// must be absolute; relative values are invalid and fall back to the standard
// per-user location, which also works on headless systems.
func flarStateDir(home string) string {
	if stateHome := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(stateHome) {
		return filepath.Join(stateHome, "flar")
	}
	return filepath.Join(home, ".local", "state", "flar")
}

func seedCodexStore(src, dst, cwd string) error {
	ids, err := copyCodexState(filepath.Join(src, codexStateDB), filepath.Join(dst, codexStateDB), cwd)
	if err != nil {
		return err
	}
	if err := copyCodexSessions(filepath.Join(src, "sessions"), filepath.Join(dst, "sessions"), cwd, ids); err != nil {
		return err
	}
	return filterCodexHistory(filepath.Join(src, "history.jsonl"), filepath.Join(dst, "history.jsonl"), ids)
}

func copyCodexState(src, dst, cwd string) (map[string]bool, error) {
	ids := map[string]bool{}
	if !fileExists(src) {
		return ids, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, err
	}
	// A live Codex normally has a WAL. VACUUM INTO takes a consistent snapshot
	// including committed WAL pages; copying only the main file can lose threads.
	srcDB, err := openSQLite(src)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(dst)
	if _, err := srcDB.Exec(`VACUUM INTO ?`, dst); err != nil {
		srcDB.Close()
		return nil, err
	}
	if err := srcDB.Close(); err != nil {
		return nil, err
	}
	db, err := openSQLite(dst)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM threads WHERE cwd = ?`, cwd)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids[id] = true
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`DELETE FROM thread_dynamic_tools WHERE thread_id NOT IN (SELECT id FROM threads WHERE cwd = ?); DELETE FROM thread_spawn_edges WHERE parent_thread_id NOT IN (SELECT id FROM threads WHERE cwd = ?) OR child_thread_id NOT IN (SELECT id FROM threads WHERE cwd = ?); DELETE FROM threads WHERE cwd <> ?`, cwd, cwd, cwd, cwd); err != nil {
		return nil, err
	}
	// These global tables are unrelated to local resume and may contain data from
	// other projects or accounts. Ignore tables absent in older Codex versions.
	for _, table := range []string{"agent_job_items", "agent_jobs", "remote_control_enrollments", "external_agent_config_imports", "backfill_state"} {
		_, _ = db.Exec(`DELETE FROM ` + table)
	}
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return ids, nil
}

type codexSessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"payload"`
}

func codexSession(path string) (codexSessionMeta, error) {
	var meta codexSessionMeta
	f, err := os.Open(path)
	if err != nil {
		return meta, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return meta, err
	}
	err = json.Unmarshal(line, &meta)
	return meta, err
}

func copyCodexSessions(src, dst, cwd string, ids map[string]bool) error {
	if !dirExists(src) {
		return os.MkdirAll(dst, 0o700)
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		meta, err := codexSession(path)
		if err != nil || meta.Type != "session_meta" || meta.Payload.Cwd != cwd {
			return nil
		}
		ids[meta.Payload.ID] = true
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return CopyFile(path, target)
	})
}

func filterCodexHistory(src, dst string, ids map[string]bool) error {
	in, err := os.Open(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	s := bufio.NewScanner(in)
	for s.Scan() {
		var row struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(s.Bytes(), &row) == nil && ids[row.SessionID] {
			if _, err := out.Write(append(append([]byte{}, s.Bytes()...), '\n')); err != nil {
				return err
			}
		}
	}
	return s.Err()
}
