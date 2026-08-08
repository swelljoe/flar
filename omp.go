package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ompSkipCopy lists paths under ~/.omp/agent (relative to it) that flar does
// NOT copy into the sandbox config. omp stores sessions in a global directory
// keyed by sha256(canonical-cwd), so sessions for other projects live alongside
// this project's. history.db is a global SQLite database with an FTS5 index
// for prompt recall, and blobs/ is a content-addressed store shared across all
// sessions. Copying any of these would leak every other project's conversation
// data into the sandbox. agent.db holds OAuth/API key credentials and is also
// skipped so the sandbox must re-authenticate via env vars or its own .env.
// The config.yml, .env, and skills/ dirs are the only user-facing config that
// should be copied verbatim.
var ompSkipCopy = map[string]bool{
	"sessions":       true,
	"history.db":     true,
	"history.db-wal": true,
	"history.db-shm": true,
	"blobs":          true,
	"agent.db":       true,
	"agent.db-wal":   true,
	"agent.db-shm":   true,
	"caches":         true,
}

// ompStoreRel is the path relative to the agent data dir (~/.omp/agent) that
// flar uses for its per-workspace shadow stores. It must never be exposed to
// another workspace's sandbox.
const ompStoreRel = ".flar"

// ompAgentDir returns the host-side omp agent data directory. omp stores
// agent data (sessions, history, credentials) under ~/.omp/agent/.
func ompAgentDir(home string) string {
	return filepath.Join(home, ".omp", "agent")
}

// ompEnvVarsForModel returns the credential environment variables omp needs
// for the given model/provider, or all omp-relevant vars when model is empty.
// The caller should merge these with the common env vars; only the returned
// vars are forwarded so the sandbox sees exactly the keys it needs.
//
// omp supports 60+ providers. When -model is not specified, all known env
// vars are forwarded — this is the permissive default matching existing
// flar behaviour. When -model is specified, only the vars for that provider
// are forwarded, limiting the blast radius of a prompt-injection attack.
func ompEnvVarsForModel(model string) []string {
	// When model is empty, return all known vars (permissive mode).
	if model == "" {
		return []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_FOUNDRY_API_KEY",
			"ANTHROPIC_SEARCH_API_KEY", "OPENAI_API_KEY", "OPENAI_CODEX_OAUTH_TOKEN",
			"GEMINI_API_KEY", "GOOGLE_CLOUD_API_KEY", "AZURE_OPENAI_API_KEY",
			"XAI_API_KEY", "OPENROUTER_API_KEY", "QWEN_OAUTH_TOKEN",
			"QWEN_PORTAL_API_KEY", "CURSOR_ACCESS_TOKEN", "HUGGINGFACE_HUB_TOKEN",
			"HF_TOKEN", "OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN",
		}
	}
	low := strings.ToLower(model)
	var out []string
	add := func(vars ...string) {
		out = append(out, vars...)
	}
	switch low {
	case "anthropic", "claude":
		add("ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_SEARCH_API_KEY")
	case "openai", "gpt":
		add("OPENAI_API_KEY", "OPENAI_CODEX_OAUTH_TOKEN")
	case "google", "gemini":
		add("GEMINI_API_KEY", "GOOGLE_CLOUD_API_KEY")
	case "azure":
		add("AZURE_OPENAI_API_KEY")
	case "xai", "grok":
		add("XAI_API_KEY")
	case "openrouter":
		add("OPENROUTER_API_KEY")
	case "qwen":
		add("QWEN_OAUTH_TOKEN", "QWEN_PORTAL_API_KEY")
	case "cursor":
		add("CURSOR_ACCESS_TOKEN")
	case "huggingface", "hf":
		add("HUGGINGFACE_HUB_TOKEN", "HF_TOKEN")
	case "broker":
		// Broker-only mode: only broker vars are needed.
		add("OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN")
		return out
	}
	// Broker vars are always needed for credential resolution regardless
	// of which model the user selects.
	add("OMP_AUTH_BROKER_URL", "OMP_AUTH_BROKER_TOKEN")
	return out
}

// prepareOmpStore returns a persistent, project-only omp agent home. It is
// seeded once from the trusted host store and never merged back: after omp
// runs in the sandbox every file in this directory must be treated as
// attacker-controlled. The store is bind-mounted as ~/.omp/agent inside the
// sandbox, so sessions created in flar persist and can be resumed with
// `omp --resume` / `--continue`, while other projects' sessions stay invisible.
func prepareOmpStore(hostHome, absProjectDir, configSrc string) (string, error) {
	hostAgent := ompAgentDir(hostHome)
	store := filepath.Join(flarStateDir(hostHome), "omp", claudeProjectSlug(absProjectDir))

	if err := os.MkdirAll(store, 0o700); err != nil {
		return "", err
	}

	marker := filepath.Join(store, ".seeded")
	if fileExists(marker) {
		return store, nil
	}

	if err := copyOmpConfig(configSrc, store); err != nil {
		return "", err
	}
	if err := seedOmpStore(hostAgent, store, absProjectDir); err != nil {
		return "", err
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return "", err
	}
	return store, nil
}

// copyOmpConfig copies the allowlisted config files from the temp config dir
// (prepared by PrepareConfigDir) into the shadow store. This includes config.yml
// and .env so the sandboxed omp can authenticate and respect user settings.
func copyOmpConfig(configSrc, store string) error {
	if configSrc == "" {
		return nil
	}
	entries, err := os.ReadDir(configSrc)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		srcPath := filepath.Join(configSrc, entry.Name())
		dstPath := filepath.Join(store, entry.Name())
		if entry.IsDir() {
			if err := CopyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := CopyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedOmpStore copies this project's existing host sessions, history rows, and
// blobs into the scoped store. Sessions are stored under
// ~/.omp/agent/sessions/<sha256(canonical-cwd)>/, keyed by the working
// directory hash, so only the hash matching absProjectDir is copied. The
// history.db SQLite database is filtered to rows whose cwd matches the current
// project. Blobs referenced by the scoped sessions are copied from the host
// blob store so the sandboxed agent can replay full conversations.
func seedOmpStore(hostAgent, store, absProjectDir string) error {
	hostSessions := filepath.Join(hostAgent, "sessions")

	// Compute the per-project session dir name: sha256 of the canonical cwd,
	// exactly as omp does.
	projectHash := fmt.Sprintf("%x", sha256.Sum256([]byte(absProjectDir)))

	// Copy this project's session directory into the shadow store.
	hostProjSessions := filepath.Join(hostSessions, projectHash)
	if dirExists(hostProjSessions) {
		dstProjSessions := filepath.Join(store, "sessions", projectHash)
		if err := os.MkdirAll(filepath.Dir(dstProjSessions), 0o700); err != nil {
			return err
		}
		if err := CopyDir(hostProjSessions, dstProjSessions); err != nil {
			return err
		}
	} else {
		// Ensure the sessions dir exists even when empty so omp's session
		// manager doesn't error on a missing directory.
		if err := os.MkdirAll(filepath.Join(store, "sessions"), 0o700); err != nil {
			return err
		}
	}

	// Filter history.db to this project's rows.
	hostHistory := filepath.Join(hostAgent, "history.db")
	if fileExists(hostHistory) {
		dstHistory := filepath.Join(store, "history.db")
		if err := filterOmpHistory(hostHistory, dstHistory, absProjectDir); err != nil {
			return err
		}
	}

	// Copy blobs referenced by this project's sessions so the sandboxed
	// agent can replay full conversations including image attachments.
	if err := seedOmpBlobs(hostAgent, store, projectHash); err != nil {
		return err
	}

	return nil
}

// filterOmpHistory creates a shadow copy of omp's history.db containing only
// the prompt history rows belonging to absProjectDir. It works by:
//  1. VACUUM INTO to snapshot the host database (consistent including WAL).
//  2. Deleting all rows whose cwd does not match the current project.
//  3. Dropping the FTS5 index and recreating it so it reflects the filtered data.
//  4. VACUUM to reclaim space.
func filterOmpHistory(src, dst, absProjectDir string) error {
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

	// Delete rows not attributed to this project. The history table has a
	// cwd column that stores the absolute path of the working directory at
	// the time the prompt was issued.
	if _, err := db.Exec(`DELETE FROM history WHERE cwd <> ?`, absProjectDir); err != nil {
		return err
	}

	// The FTS5 index (history_fts) must be rebuilt after deletion.
	// Try to drop and recreate it; ignore errors if the index doesn't exist
	// in this version of omp.
	_, _ = db.Exec(`DROP INDEX IF EXISTS history_fts_idx`)
	_, _ = db.Exec(`DROP TABLE IF EXISTS history_fts`)
	_, _ = db.Exec(`CREATE VIRTUAL TABLE history_fts USING fts5(prompt, content=cwd, content_rowid='id')`)
	_, _ = db.Exec(`INSERT INTO history_fts(rowid, prompt, content) SELECT id, prompt, cwd FROM history`)

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

// seedOmpBlobs copies blob files referenced by this project's sessions into
// the shadow store. omp stores binary attachments (images, etc.) under
// ~/.omp/agent/blobs/<sha256> using content-addressed storage. Blobs are
// shared across sessions, so we only copy the ones actually referenced by
// this project's session files to avoid bloating the shadow store.
func seedOmpBlobs(hostAgent, store, projectHash string) error {
	hostBlobs := filepath.Join(hostAgent, "blobs")
	if !dirExists(hostBlobs) {
		return nil
	}
	storeBlobs := filepath.Join(store, "blobs")
	if err := os.MkdirAll(storeBlobs, 0o700); err != nil {
		return nil
	}

	// Read the session JSONL file to find blob references. Session files are
	// stored as <timestamp>_<sessionId>.jsonl under the project hash dir.
	sessionDir := filepath.Join(hostAgent, "sessions", projectHash)
	if !dirExists(sessionDir) {
		return nil
	}

	refs := make(map[string]bool)
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			data, err := os.ReadFile(filepath.Join(sessionDir, entry.Name()))
			if err != nil {
				continue
			}
			// Scan for blob SHA-256 references in the session JSONL.
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if matches := findBlobRefs(line); len(matches) > 0 {
					for _, ref := range matches {
						refs[ref] = true
					}
				}
			}
		}
	}

	// Copy referenced blobs.
	for ref := range refs {
		src := filepath.Join(hostBlobs, ref)
		dst := filepath.Join(storeBlobs, ref)
		if fileExists(src) {
			if err := CopyFile(src, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

// findBlobRefs scans a JSON line for SHA-256 blob references. omp stores
// blob references in various fields; we look for 64-char hex strings that
// resemble SHA-256 digests in blob-related contexts.
func findBlobRefs(line string) []string {
	var refs []string
	const hex = "0123456789abcdef"
	for i := 0; i+len(hex) <= len(line); i++ {
		potential := line[i : i+64]
		isHex := true
		for _, c := range potential {
			if !strings.ContainsRune(hex, c) {
				isHex = false
				break
			}
		}
		if !isHex {
			continue
		}
		contextStart := i - 30
		if contextStart < 0 {
			contextStart = 0
		}
		context := line[contextStart:i]
		if strings.Contains(context, `"blob"`) || strings.Contains(context, `"sha256"`) ||
			strings.Contains(context, `"blobRef"`) || strings.Contains(context, `"contentRef"`) {
			refs = append(refs, potential)
		}
	}
	return refs
}
