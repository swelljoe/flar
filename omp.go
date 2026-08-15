package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ompAgentDirName is the omp agent home directory under $HOME. It holds the
// session transcripts, global prompt history, credential store, and config.
const ompAgentDirName = ".omp/agent"

// ompProviderEnvVars maps provider keys (as stored in agent.db auth_credentials
// and used in model selectors) to the env vars omp reads for that provider's
// credentials. Used to scope which host env vars enter the sandbox.
var ompProviderEnvVars = map[string][]string{
	"anthropic":       {"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN"},
	"openai":          {"OPENAI_API_KEY"},
	"gpt5":            {"OPENAI_API_KEY"},
	"gemini":          {"GEMINI_API_KEY"},
	"moonshot":        {"MOONSHOT_API_KEY", "KIMI_API_KEY"},
	"kimi-for-coding": {"MOONSHOT_API_KEY", "KIMI_API_KEY"},
	"xiaomi":          {"XIAOMI_API_KEY", "XIAOMI_TOKEN_PLAN_BASE_URL", "XIAOMI_TOKEN_PLAN_KEY_ID", "XIAOMI_TOKEN_PLAN_ACCOUNT_ID", "XIAOMI_TOKEN_PLAN_REGION"},
	"deepseek":        {"DEEPSEEK_API_KEY"},
	"exa":             {"EXA_API_KEY"},
	"llama.cpp":       {"LLAMA_CPP_API_KEY", "LLAMA_CPP_BASE_URL"},
	"openrouter":      {"OPENROUTER_API_KEY"},
	"zai":             {"ZAI_API_KEY"},
	"perplexity":      {"PERPLEXITY_API_KEY"},
	"brave":           {"BRAVE_API_KEY"},
	"tavily":          {"TAVILY_API_KEY"},
	"firecrawl":       {"FIRECRAWL_API_KEY"},
	"jina":            {"JINA_API_KEY"},
	"kagi":            {"KAGI_API_KEY"},
	"groq":            {"GROQ_API_KEY"},
	"mistral":         {"MISTRAL_API_KEY"},
	"minimax":         {"MINIMAX_API_KEY", "MINIMAX_BASE_URL"},
	"nvidia":          {"NVIDIA_API_KEY"},
	"siliconflow":     {"SILICONFLOW_API_KEY", "SILICONFLOW_BASE_URL"},
	"huggingface":     {"HF_TOKEN", "HUGGINGFACE_HUB_TOKEN"},
	"ollama":          {"OLLAMA_API_KEY"},
}

// ompHostEnvPrefixes: env vars with these prefixes are forwarded as
// model/role configuration (PI_SMOL_MODEL, OMP_PROFILE, ...). PI_PROXY* and
// PI_OAUTH_* must never be forwarded.
var ompHostEnvPrefixes = []string{"OMP_", "PI_"}

// resolvedOmpEnvVars computes which host env vars carry omp credentials/config.
// When providers is non-empty, only that provider's credential vars are kept;
// otherwise all known provider credential vars present in the host env are kept
// (unscoped). PI_PROXY* and PI_OAUTH_* are always excluded. The second return
// value reports whether scoping was applied.
func resolvedOmpEnvVars(providers []string) (map[string]string, bool) {
	out := make(map[string]string)
	scoped := false
	allowed := make([]string, 0, len(providers))
	for _, p := range providers {
		if p = strings.TrimSpace(p); p != "" {
			allowed = append(allowed, p)
			scoped = true
		}
	}
	if !scoped {
		// Unscoped: forward every known provider credential var present in
		// the host env. The caller warns that this exposes all providers.
		for _, vars := range ompProviderEnvVars {
			for _, v := range vars {
				if val, ok := os.LookupEnv(v); ok {
					out[v] = val
				}
			}
		}
	}
	for _, p := range allowed {
		for _, v := range ompProviderEnvVars[p] {
			if val, ok := os.LookupEnv(v); ok {
				out[v] = val
			}
		}
	}
	for _, kv := range os.Environ() {
		eq := strings.Index(kv, "=")
		if eq < 0 {
			continue
		}
		name, val := kv[:eq], kv[eq+1:]
		if !hasAnyPrefix(name, ompHostEnvPrefixes) {
			continue
		}
		if strings.HasPrefix(name, "PI_PROXY") || strings.HasPrefix(name, "PI_OAUTH_") {
			continue
		}
		out[name] = val
	}
	return out, scoped
}

// resolveOmpProviders maps the user's -omp-model specification to provider
// keys for credential scoping. Accepts a provider key ("deepseek") or a model
// selector ("provider/model", with optional fuzzy/namespace parts); only the
// first path segment matters, since it identifies the provider. Returns nil
// when no valid specification is given (unscoped mode; caller warns).
func resolveOmpProviders(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	provider := model
	if i := strings.Index(model, "/"); i > 0 {
		provider = model[:i]
	}
	provider = strings.ToLower(provider)
	// Match known providers case-insensitively; unknown names pass through so
	// custom models.yml providers still scope by their literal key.
	for known := range ompProviderEnvVars {
		if strings.ToLower(known) == provider {
			return []string{known}
		}
	}
	return []string{provider}
}

// ompSessionBucket replicates the session bucket naming from omp's
// session-paths.ts (computeDefaultSessionDir): a cwd under $HOME encodes as
// -<home-relative>, a cwd under os.TempDir() as -tmp-<tmp-relative>, and
// everything else as --<absolute with leading separator stripped>--. Like
// omp, the cwd and both roots are resolved through realpath before comparing.
// The encoding is NOT injective (a/b and a-b collide); flar never uses bucket
// names to scope data — session selection follows the cwd each transcript
// self-attests.
func ompSessionBucket(home, absDir string) string {
	cwd := ompRealPath(absDir)
	if rel, ok := ompRelPath(ompRealPath(home), cwd); ok {
		return ompJoinBucket("-", rel)
	}
	if rel, ok := ompRelPath(ompRealPath(os.TempDir()), cwd); ok {
		return ompJoinBucket("-tmp", rel)
	}
	abs := cwd
	if len(abs) > 1 {
		abs = strings.TrimLeft(abs, "/\\")
	}
	return "--" + ompSanitizePathPart(abs) + "--"
}

// ompRealPath mirrors omp's Pa(): resolve then realpath, falling back to the
// input when the path does not exist (realpath fails).
func ompRealPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// ompRelPath returns the relative path from root to p when p is inside root,
// mapping an identical path to "" (node's path.relative returns "" there).
func ompRelPath(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return rel, true
}

// ompJoinBucket implements omp's co_(): prefix + sanitized relative path, with
// a single "-" between them — or direct concatenation when the prefix already
// ends in "-", and the bare prefix when the relative part is empty (the cwd
// equals the root).
func ompJoinBucket(prefix, rel string) string {
	part := ompSanitizePathPart(rel)
	if part == "" {
		return prefix
	}
	if strings.HasSuffix(prefix, "-") {
		return prefix + part
	}
	return prefix + "-" + part
}

// ompSanitizePathPart replaces every /, \, and : with "-", mirroring the
// regex /[/\\:]/g in omp's encoder.
func ompSanitizePathPart(s string) string {
	return strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(s)
}

// cwdAttestation extracts the self-attested project cwd from an omp session's
// first .jsonl file (the session header record carries "cwd"). Returns "" when
// no attestation is found.
func cwdAttestation(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return "", err
		}
		hdr := make([]byte, 8192)
		n, _ := f.Read(hdr)
		f.Close()
		for _, line := range strings.Split(string(hdr[:n]), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var rec struct {
				Type string `json:"type"`
				Cwd  string `json:"cwd"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if rec.Type == "session" && rec.Cwd != "" {
				return rec.Cwd, nil
			}
		}
	}
	return "", nil
}

// copyOmpSessions seeds the shadow store's sessions/ directory with session
// files self-attested as belonging to absProjectDir, taken from all host
// buckets. Sessions created inside the sandbox land in the shadow bucket and
// persist there on subsequent runs.
func copyOmpSessions(hostAgentDir, store, home, absProjectDir string) error {
	hostSess := filepath.Join(hostAgentDir, "sessions")
	entries, err := os.ReadDir(hostSess)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bucketPath := filepath.Join(hostSess, e.Name())
		cwd, err := cwdAttestation(bucketPath)
		if err != nil || cwd != absProjectDir {
			continue
		}
		dstBucket := filepath.Join(store, "sessions", ompSessionBucket(home, cwd))
		if err := copyOmpDir(bucketPath, dstBucket); err != nil {
			return fmt.Errorf("copy session bucket %s: %w", e.Name(), err)
		}
	}
	return nil
}

// forkOmpHistoryDB forks the global prompt history DB into the shadow store,
// keeping only rows whose cwd matches absProjectDir. The FTS5 index is rebuilt
// so deleted terms no longer match, and the WAL is checkpointed so a plain bind
// of the main file works without leftover .wal/.shm.
func forkOmpHistoryDB(hostAgentDir, store, absProjectDir string) error {
	src := filepath.Join(hostAgentDir, "history.db")
	if _, err := os.Stat(src); err != nil {
		return nil // no history yet; nothing to fork
	}
	dst := filepath.Join(store, "history.db")
	// A live omp normally keeps the DB in WAL mode. VACUUM INTO from the source
	// connection takes a consistent snapshot including committed WAL pages.
	srcDB, err := openSQLite(src)
	if err != nil {
		return fmt.Errorf("open history db: %w", err)
	}
	_ = os.Remove(dst)
	if _, err := srcDB.Exec("VACUUM INTO ?", dst); err != nil {
		srcDB.Close()
		return fmt.Errorf("vacuum into: %w", err)
	}
	srcDB.Close()
	db, err := openSQLite(dst)
	if err != nil {
		return fmt.Errorf("open forked history db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM history WHERE cwd <> ? OR cwd IS NULL", absProjectDir); err != nil {
		return fmt.Errorf("filter history by cwd: %w", err)
	}
	if _, err := db.Exec("INSERT INTO history_fts(history_fts) VALUES('rebuild')"); err != nil {
		return fmt.Errorf("rebuild FTS index: %w", err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint forked history db: %w", err)
	}
	return nil
}

// forkOmpAgentDB forks the global agent DB (credential store and usage
// telemetry) into the shadow store. When providers is non-empty, auth_credentials
// rows for other providers are removed along with their cache/lease/block rows;
// otherwise credentials are kept and the caller emits an exposure warning.
// Telemetry tables are always cleared so the shadow starts clean.
func forkOmpAgentDB(hostAgentDir, store string, providers []string) error {
	src := filepath.Join(hostAgentDir, "agent.db")
	if _, err := os.Stat(src); err != nil {
		return nil // no agent db yet; nothing to fork
	}
	dst := filepath.Join(store, "agent.db")
	srcDB, err := openSQLite(src)
	if err != nil {
		return fmt.Errorf("open agent db: %w", err)
	}
	_ = os.Remove(dst)
	if _, err := srcDB.Exec("VACUUM INTO ?", dst); err != nil {
		srcDB.Close()
		return fmt.Errorf("vacuum into: %w", err)
	}
	srcDB.Close()
	db, err := openSQLite(dst)
	if err != nil {
		return fmt.Errorf("open forked agent db: %w", err)
	}
	if len(providers) > 0 {
		marks, vals := "(", make([]any, 0, len(providers))
		for _, p := range providers {
			marks += "?"
			vals = append(vals, p)
			marks += ","
		}
		marks = marks[:len(marks)-1] + ")"
		var removedIDs []any
		rows, err := db.Query("SELECT id FROM auth_credentials WHERE provider NOT IN "+marks, vals...)
		if err != nil {
			return fmt.Errorf("select foreign credentials: %w", err)
		}
		for rows.Next() {
			var id any
			if err := rows.Scan(&id); err == nil {
				removedIDs = append(removedIDs, id)
			}
		}
		rows.Close()
		if len(removedIDs) > 0 {
			inList := "(" + strings.Repeat("?,", len(removedIDs)-1) + "?)"
			for _, table := range []string{
				"auth_credential_refresh_leases",
				"auth_credential_blocks",
				"auth_credential_block_mirror_guard",
			} {
				if _, err := db.Exec("DELETE FROM "+table+" WHERE credential_id IN "+inList, removedIDs...); err != nil {
					return fmt.Errorf("clean %s: %w", table, err)
				}
			}
		}
		if _, err := db.Exec("DELETE FROM auth_credentials WHERE provider NOT IN "+marks, vals...); err != nil {
			return fmt.Errorf("filter credentials by provider: %w", err)
		}
	}
	for _, table := range []string{"cache", "clients", "client_usage", "usage_cost_history", "usage_history", "model_perf", "model_usage"} {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint forked agent db: %w", err)
	}
	return nil
}

// prepareOmpStore returns the per-project shadow store path for omp state. On
// first use it seeds from the host's ~/.omp/agent: session transcripts filtered
// by internal cwd attestation, a project-filtered history.db fork, an agent.db
// fork scoped to providers (empty = unscoped; caller warns), and the config
// files copied from tempConfig. The store persists in flar's state dir so
// sandbox-created sessions remain available on the next run.
func prepareOmpStore(hostHome, absProjectDir string, providers []string, tempConfig string) (string, error) {
	hostAgentDir := filepath.Join(hostHome, ompAgentDirName)
	if _, err := os.Stat(hostAgentDir); err != nil {
		return "", fmt.Errorf("omp agent dir not found: %s", hostAgentDir)
	}
	store := filepath.Join(flarStateDir(hostHome), "omp", claudeProjectSlug(absProjectDir))
	if err := os.MkdirAll(store, 0700); err != nil {
		return "", err
	}
	marker := filepath.Join(store, ".seeded")
	if _, err := os.Stat(marker); err == nil {
		return store, nil // already seeded; shadow is authoritative
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := copyOmpSessions(hostAgentDir, store, hostHome, absProjectDir); err != nil {
		return "", fmt.Errorf("copy omp sessions: %w", err)
	}
	if err := forkOmpHistoryDB(hostAgentDir, store, absProjectDir); err != nil {
		return "", fmt.Errorf("fork omp history db: %w", err)
	}
	if err := forkOmpAgentDB(hostAgentDir, store, providers); err != nil {
		return "", fmt.Errorf("fork omp agent db: %w", err)
	}
	if err := copyOmpConfigFiles(tempConfig, store); err != nil {
		return "", fmt.Errorf("copy omp config files: %w", err)
	}
	if err := os.WriteFile(marker, []byte("seeded\n"), 0600); err != nil {
		return "", err
	}
	return store, nil
}

// copyOmpConfigFiles copies the filtered config files (config.yml, models.yml)
// from the temp config dir into the shadow store. The temp dir is prepared by
// PrepareConfigDir and never contains .env or database files.
func copyOmpConfigFiles(tempConfig, store string) error {
	for _, name := range []string{"config.yml", "models.yml"} {
		src := filepath.Join(tempConfig, "omp-config", name)
		if _, err := os.Stat(src); err != nil {
			continue // optional; host may have no config yet
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(store, name), data, 0600); err != nil {
			return err
		}
	}
	return nil
}

// copyOmpDir recursively copies a directory tree with restrictive permissions.
func copyOmpDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		switch {
		case e.IsDir():
			if err := copyOmpDir(srcPath, dstPath); err != nil {
				return err
			}
		case e.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			os.Remove(dstPath)
			if err := os.Symlink(target, dstPath); err != nil {
				return err
			}
		default:
			data, err := os.ReadFile(srcPath)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dstPath, data, 0600); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasAnyPrefix reports whether name starts with any of the given prefixes.
func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
