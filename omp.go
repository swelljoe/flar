package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ompStoreDirs are the omp subdirectories under ~/.omp/agent/ that flar
// replaces at run time with a project-scoped shadow store:
//   - sessions: per-project session files (JSONL). Already project-scoped on
//     disk by directory, but the directory also contains sessions for other
//     projects.
//   - history.db: global SQLite database mixing all projects' prompt history.
//
// blobs/ (content-addressed, shared), config.yml, models.yml, and agent.db
// are safe to copy and are not in this list.
var ompStoreDirs = []string{"sessions", "history.db"}

// prepareOmpStore returns the path to this workspace's private omp store,
// creating it and — once — seeding it from the host's global store. The store
// is bind-mounted over ~/.omp/agent/sessions and ~/.omp/agent/history.db inside
// the sandbox so sessions created in flar persist and can be resumed, while
// other projects' sessions and history stay invisible.
//
// omp already scopes sessions per-project on disk (each project gets its own
// subdirectory under sessions/), but the directory also contains sessions for
// every other project. The history.db is a single global SQLite database that
// mixes all projects. flar forks both into a per-project shadow store here.
//
// The host store is read once at seed time; after the one-time seed the store
// diverges: new flar sessions accumulate here and later host-side changes are
// not pulled in.
func prepareOmpStore(hostHome, absProjectDir string) (string, error) {
	hostStore := filepath.Join(hostHome, ".omp", "agent")
	store := filepath.Join(flarStateDir(hostHome), "omp", claudeProjectSlug(absProjectDir))

	if err := os.MkdirAll(filepath.Join(store, "sessions"), 0o700); err != nil {
		return "", err
	}

	// Seed exactly once; the marker guards against re-seeding (which would
	// clobber sessions the user has since continued inside flar).
	marker := filepath.Join(store, ".seeded")
	if _, err := os.Stat(marker); err != nil {
		seedOmpStore(hostStore, store, absProjectDir) // best-effort
		if f, err := os.OpenFile(marker, os.O_CREATE, 0o600); err == nil {
			f.Close()
		}
	}

	// Ensure the history.db exists (even if empty) so it can be bind-mounted.
	ensureFile(filepath.Join(store, "history.db"))
	return store, nil
}

// seedOmpStore copies this workspace's existing host sessions and history into
// the scoped store.
func seedOmpStore(hostStore, store, absProjectDir string) {
	seedOmpSessions(hostStore, store, absProjectDir)
	seedOmpHistory(hostStore, store, absProjectDir)
}

// seedOmpSessions copies only session files attributed to absProjectDir from
// the host's sessions directory into the scoped store. omp stores sessions
// under project-scoped subdirectories, but a single directory may contain
// sessions from multiple projects (e.g., after a cwd migration). Each session
// file's header records the owning cwd, so we check the header rather than
// relying on the directory name.
func seedOmpSessions(hostStore, store, absProjectDir string) {
	hostSessions := filepath.Join(hostStore, "sessions")
	if _, err := os.Stat(hostSessions); err != nil {
		return
	}

	entries, err := os.ReadDir(hostSessions)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		hostDir := filepath.Join(hostSessions, entry.Name())
		destDir := filepath.Join(store, "sessions", entry.Name())

		files, err := os.ReadDir(hostDir)
		if err != nil {
			continue
		}

		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			hostFile := filepath.Join(hostDir, f.Name())
			if ompSessionMatchesCwd(hostFile, absProjectDir) {
				if err := os.MkdirAll(destDir, 0o700); err != nil {
					continue
				}
				_ = CopyFile(hostFile, filepath.Join(destDir, f.Name()))
			}
		}
	}
}

// ompSessionMatchesCwd reads the first two lines of a session JSONL file and
// checks whether the header's cwd field matches absProjectDir. The header is
// the first JSON object with type "session". In newer omp formats, the file
// begins with a fixed-width 256-byte title slot (a JSON object with type
// "title"), so the session header may be on the second line.
func ompSessionMatchesCwd(sessionFile, absProjectDir string) bool {
	f, err := os.Open(sessionFile)
	if err != nil {
		return false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for i := 0; i < 2 && sc.Scan(); i++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var header struct {
			Type string `json:"type"`
			Cwd  string `json:"cwd"`
		}
		if json.Unmarshal([]byte(line), &header) != nil {
			continue
		}
		if header.Type == "session" {
			return header.Cwd == absProjectDir
		}
	}
	return false
}

// seedOmpHistory writes a scoped history.db containing only prompt-history
// rows attributed to absProjectDir. omp stores prompt history in a global
// SQLite database (history.db) with a history table (id, prompt, created_at,
// cwd, session_id) and an FTS5 index (history_fts).
func seedOmpHistory(hostStore, store, absProjectDir string) {
	hostDB := filepath.Join(hostStore, "history.db")
	if _, err := os.Stat(hostDB); err != nil {
		return
	}

	dstDB := filepath.Join(store, "history.db")
	if err := filterOmpHistoryDB(hostDB, dstDB, absProjectDir); err != nil {
		// If filtering fails, fall back to an empty database rather than
		// risking cross-project contamination.
		_ = os.WriteFile(dstDB, []byte("SQLite format 3\x00"), 0o600)
	}
}

// filterOmpHistoryDB creates a shadow copy of the omp history database
// containing only rows whose cwd matches absProjectDir. It works by:
//  1. VACUUM INTO to snapshot the host database (consistent including WAL).
//  2. Deleting all history rows not attributed to the target project.
//  3. Clearing the FTS index.
//  4. VACUUM to reclaim space.
func filterOmpHistoryDB(src, dst, absProjectDir string) error {
	_ = os.Remove(dst)
	_ = os.Remove(dst + "-wal")
	_ = os.Remove(dst + "-shm")

	srcDB, err := openSQLite(src)
	if err != nil {
		return err
	}

	// VACUUM INTO gives a consistent snapshot including committed WAL pages.
	if _, err := srcDB.Exec(`VACUUM INTO ?`, dst); err != nil {
		srcDB.Close()
		return err
	}
	srcDB.Close()

	db, err := openSQLite(dst)
	if err != nil {
		return err
	}
	defer db.Close()

	// Delete all history rows not attributed to this project.
	if _, err := db.Exec(`DELETE FROM history WHERE cwd <> ?`, absProjectDir); err != nil {
		return err
	}

	// Clear the FTS index. The history table's triggers should handle this,
	// but we clear explicitly as defense in depth.
	_, _ = db.Exec(`DELETE FROM history_fts`)

	// Checkpoint WAL and VACUUM to compact.
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	db.Close()

	vdb, err := openSQLite(dst)
	if err != nil {
		return err
	}
	defer vdb.Close()
	_, _ = vdb.Exec(`VACUUM`)
	return nil
}

// ompAgentDir returns the host-side omp agent directory, honoring
// PI_CODING_AGENT_DIR.
func ompAgentDir(home string) string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(home, ".omp", "agent")
}

// ompEnvVarsForProviders returns the unique environment variable names needed
// to authenticate the given omp provider IDs.
func ompEnvVarsForProviders(providers []string) []string {
	seen := map[string]bool{}
	var vars []string
	for _, p := range providers {
		for _, env := range ompProviderEnvVars[p] {
			if !seen[env] {
				seen[env] = true
				vars = append(vars, env)
			}
		}
	}
	return vars
}

// ompValidateProviders checks that the given provider IDs are known to flar.
// Returns a list of unknown provider IDs.
func ompValidateProviders(providers []string) []string {
	known := map[string]bool{}
	for k := range ompProviderEnvVars {
		known[k] = true
	}
	var unknown []string
	for _, p := range providers {
		if !known[p] {
			unknown = append(unknown, p)
		}
	}
	return unknown
}
