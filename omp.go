package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ompSkipCopy lists entries under the host omp agent directory (~/.omp/agent) that
// flar does NOT copy as a static overlay. They are handled at run time instead:
//
//   - history.db*: the global prompt-history SQLite database mixes every project's
//     prompts and its picker is not cwd-scoped, so it must be forked per-project
//     (see seedOmpHistory) rather than copied wholesale.
//   - agent.db*: the centralized auth/usage store holding every provider's API key
//     plus other projects' client usage. It is either snapshot-copied or narrowed
//     to one provider at run time (see seedOmpAuth).
//   - sessions: every project's transcripts live here; only this project's slug is
//     live-bound at run time, so copying the whole directory would leak other
//     projects' sessions into the sandbox.
//   - models.db*: a model catalog cache; regenerating it inside the sandbox from
//     models.yml keeps it current and avoids WAL-sidecar copy pitfalls.
var ompSkipCopy = map[string]bool{
	"history.db":     true,
	"history.db-wal": true,
	"history.db-shm": true,
	"agent.db":       true,
	"agent.db-wal":   true,
	"agent.db-shm":   true,
	"models.db":      true,
	"models.db-wal":  true,
	"models.db-shm":  true,
	"sessions":       true,
}

// ompAgentDir returns the host-side omp agent directory that flar mirrors. omp's
// DirResolver relocates per-category state under $XDG_{DATA,STATE,CACHE}_HOME/omp
// only when those directories already exist and no profile/env override is active;
// on a default install everything lives in ~/.omp/agent. Inside the sandbox flar
// binds its shadow store to exactly this path (and does not forward XDG_DATA_HOME),
// so omp always resolves here regardless of any host-side migration.
func ompAgentDir(home string) string {
	if override := os.Getenv("PI_CONFIG_DIR"); override != "" {
		return filepath.Join(override, "agent")
	}
	return filepath.Join(home, ".omp", "agent")
}

// resolveOmpPath mirrors omp's resolveEquivalentPath (dirs.ts): path.resolve then
// fs.realpathSync, falling back to the resolved path when realpath fails. flar runs
// only on Linux for bwrap, so no darwin/win32 branching is needed.
func resolveOmpPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	// EvalSymlinks resolves the full path (all symlink components), matching Node's
	// fs.realpathSync; fall back to the plain absolute path when it cannot.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// ompWithin mirrors Node's "relative(root, cand) is under root" check used by
// getDefaultSessionDirName: true when the relative path is empty (same path) or neither
// escapes the root ("..") nor absolute. Go's filepath.Rel returns "." for the same path
// where Node returns "", so "." is treated as within.
func ompWithin(rel string) bool {
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// encodeRelativeSlug mirrors omp's encodeRelativeSessionDirName: the prefix joined to the
// relative path with "/" "\" ":" replaced by "-". A collapsed relative (Go's "." for the
// same path) yields just the prefix; prefixes that already end in "-" are joined without
// an extra separator.
func encodeRelativeSlug(prefix, rel string) string {
	if rel == "." {
		return prefix
	}
	encoded := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(rel)
	if encoded == "" {
		return prefix
	}
	if strings.HasSuffix(prefix, "-") {
		return prefix + encoded
	}
	return prefix + "-" + encoded
}

// encodeOmpSessionSlug reproduces omp's getDefaultSessionDirName (session-paths.ts).
// Sessions are grouped by cwd scope: home-relative → "-<homeRelative>", temp-relative
// → "-tmp-<tempRelative>", otherwise absolute "--<abs>--". For the common case of a
// project under $HOME this yields e.g. "/home/joe/src/flar" → "-src-flar", which is
// deliberately different from claudeProjectSlug (an absolute-path encoding) — omp's
// own on-disk session directories use this scheme, so flar must match it to locate
// the project's existing sessions for resume.
func encodeOmpSessionSlug(cwd string) string {
	canonicalCwd := resolveOmpPath(cwd)
	home, _ := os.UserHomeDir()
	canonicalHome := resolveOmpPath(home)

	if rel, err := filepath.Rel(canonicalHome, canonicalCwd); err == nil && ompWithin(rel) {
		return encodeRelativeSlug("-", rel)
	}

	canonicalTmp := resolveOmpPath(os.TempDir())
	if rel, err := filepath.Rel(canonicalTmp, canonicalCwd); err == nil && ompWithin(rel) {
		return encodeRelativeSlug("-tmp", rel)
	}

	stripped := strings.TrimPrefix(canonicalCwd, "/")
	return "--" + strings.ReplaceAll(stripped, "/", "-") + "--"
}

// prepareOmpStore returns a persistent, project-only shadow of the omp agent
// directory under $XDG_STATE_HOME/flar/omp/<slug>/. It is seeded once from the
// trusted host store and never merged back: after omp runs in the sandbox every
// file in this directory must be treated as attacker-controlled.
//
// The shadow overlays ~/.omp/agent inside the sandbox. Centralized state that mixes
// projects is isolated here: history.db is forked down to this project's rows (and
// its FTS index rebuilt), and agent.db is either snapshot-copied or narrowed to a
// single provider's credentials when a model is pinned with -omp-model. Sessions are
// not seeded — the current project's live session directory is bound over it at run
// time (see RunSandbox) so sessions created in flar persist and resume, while other
// projects' sessions stay invisible.
func prepareOmpStore(hostHome, absProjectDir, model string) (string, error) {
	store := filepath.Join(flarStateDir(hostHome), "omp", claudeProjectSlug(absProjectDir))
	if err := os.MkdirAll(store, 0o700); err != nil {
		return "", err
	}

	marker := filepath.Join(store, ".seeded")
	if fileExists(marker) {
		return store, nil
	}

	hostAgent := ompAgentDir(hostHome)
	if _, err := os.Stat(hostAgent); err == nil {
		if err := CopyDirExcept(hostAgent, store, ompSkipCopy); err != nil {
			return "", fmt.Errorf("copy omp config: %w", err)
		}
	}

	// Ensure an empty sessions/ root exists in the shadow so a live session-dir bind
	// has a mount point even for a brand-new project with no host sessions yet.
	if err := os.MkdirAll(filepath.Join(store, "sessions"), 0o700); err != nil {
		return "", err
	}

	if err := seedOmpHistory(hostAgent, store, absProjectDir); err != nil {
		return "", fmt.Errorf("seed omp history: %w", err)
	}
	if model != "" {
		if err := seedOmpAuth(hostAgent, store, providerFromOmpModel(model)); err != nil {
			return "", fmt.Errorf("narrow omp auth: %w", err)
		}
	} else if _, err := os.Stat(filepath.Join(store, "agent.db")); err != nil {
		// No model pin: snapshot the full agent store (credentials for every provider).
		if err := snapshotSQLiteFile(filepath.Join(hostAgent, "agent.db"), filepath.Join(store, "agent.db")); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("snapshot agent.db: %w", err)
		}
	}

	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return "", fmt.Errorf("create omp seed marker: %w", err)
	}
	return store, nil
}

// seedOmpHistory forks the host's centralized history.db into a per-project copy that
// contains only this project's rows. The picker (history-storage.ts) searches without
// a cwd filter, so an unfiltered DB would let a sandboxed prompt see every other
// project's prompts; forking by cwd removes that path. The FTS5 index is rebuilt to
// match the reduced table.
func seedOmpHistory(hostAgent, store, absProjectDir string) error {
	hostDB := filepath.Join(hostAgent, "history.db")
	dst := filepath.Join(store, "history.db")
	if _, err := os.Stat(hostDB); err != nil {
		return nil // no history yet; omp creates it on first run in the sandbox
	}

	src, err := openSQLite(hostDB)
	if err != nil {
		return err
	}
	// VACUUM INTO yields a consistent snapshot including committed WAL pages.
	if _, err := src.Exec("VACUUM INTO ?", dst); err != nil {
		src.Close()
		return err
	}
	src.Close()

	db, err := openSQLite(dst)
	if err != nil {
		return err
	}
	defer db.Close()
	// Keep only rows that belong to this project. Omp records each prompt's unsymlinked
	// absolute cwd, which equals absProjectDir on a normal (non-symlinked) checkout — the
	// same path flar live-binds as the project dir. `cwd IS NOT ?` deletes every row whose
	// cwd differs or is NULL: other projects' prompts and unattributed ones never leak in.
	if _, err := db.Exec("DELETE FROM history WHERE cwd IS NOT ?", absProjectDir); err != nil {
		return err
	}
	// Rebuild the FTS5 index so it reflects only this project's rows. Best effort:
	// omitting it keeps history searchable via a scan if rebuild is unavailable.
	if _, err := db.Exec("INSERT INTO history_fts(history_fts) VALUES('rebuild')"); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not rebuild omp history FTS index: %v\n", err)
	}
	return nil
}

// seedOmpAuth narrows the host's centralized agent.db to credentials for a single
// provider. flar copies it into the shadow and deletes every auth_credentials row
// whose provider does not match, so a sandboxed omp cannot read other providers' API
// keys (the main blast-radius path when one user has many model-provider keys). When
// no provider matches, all credentials are removed — the safe outcome.
func seedOmpAuth(hostAgent, store, provider string) error {
	srcPath := filepath.Join(hostAgent, "agent.db")
	dst := filepath.Join(store, "agent.db")
	if _, err := os.Stat(srcPath); err != nil {
		return nil
	}

	src, err := openSQLite(srcPath)
	if err != nil {
		return err
	}
	if _, err := src.Exec("VACUUM INTO ?", dst); err != nil {
		src.Close()
		return err
	}
	src.Close()

	db, err := openSQLite(dst)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM auth_credentials WHERE provider <> ?", provider); err != nil {
		return err
	}
	if _, err := db.Exec("VACUUM"); err != nil {
		return err
	}
	return nil
}

// snapshotSQLiteFile copies a host SQLite database (and its WAL/SHM sidecars) into
// the shadow when a plain VACUUM INTO is not required. Used for agent.db when no
// model is pinned so all provider credentials are available in the sandbox.
func snapshotSQLiteFile(src, dst string) error {
	db, err := openSQLite(src)
	if err != nil {
		return err
	}
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		db.Close()
		return err
	}
	db.Close()
	return nil
}

// providerFromOmpModel extracts the provider segment from an omp model id of the form
// "<provider>/<model-id>" (optionally suffixed with ":<variant>").
func providerFromOmpModel(model string) string {
	idx := strings.Index(model, "/")
	if idx <= 0 {
		return ""
	}
	return model[:idx]
}

// ompAgentEnvVars returns the credential environment variables flar forwards for the
// omp agent. When a model is pinned via -omp-model, only that provider's env vars are
// forwarded, matching the narrowed agent.db. With no pin, none are forwarded: omp
// authenticates from agent.db credentials, and forwarding every host *_API_KEY would
// defeat the point of narrowing. Callers add these to the common (non-secret) set.
func ompAgentEnvVars(model string) []string {
	if model == "" {
		return nil
	}
	provider := providerFromOmpModel(model)
	envs, ok := ompProviderEnvVars[provider]
	if !ok {
		fmt.Fprintf(os.Stderr, "Warning: no known env vars for omp provider %q; relying on agent.db credentials only.\n", provider)
		return nil
	}
	return envs
}

// ompProviderEnvVars maps an omp provider id to the environment variable(s) that
// authenticate it, mirroring pi-catalog's provider-models/descriptors.ts. Only the
// pinned model's provider is ever forwarded into the sandbox (see ompAgentEnvVars),
// so one user with many model-provider keys need not expose them all to a single
// run. Unknown providers simply get no env forwarding; their agent.db credential
// (if any) still applies.
var ompProviderEnvVars = map[string][]string{
	"aiand":                 {"AIAND_API_KEY"},
	"aimlapi":               {"AIMLAPI_API_KEY"},
	"alibaba-coding-plan":   {"ALIBABA_CODING_PLAN_API_KEY"},
	"alibaba-token-plan":    {"ALIBABA_TOKEN_PLAN_API_KEY", "BAILIAN_TOKEN_PLAN_API_KEY"},
	"amazon-bedrock":        {"AWS_BEARER_TOKEN_BEDROCK"},
	"anthropic":             {"ANTHROPIC_API_KEY"},
	"azure":                 {"AZURE_OPENAI_API_KEY"},
	"baseten":               {"BASETEN_API_KEY"},
	"cerebras":              {"CEREBRAS_API_KEY"},
	"cloudflare-ai-gateway": {"CLOUDFLARE_AI_GATEWAY_API_KEY"},
	"coreweave":             {"COREWEAVE_API_KEY", "WANDB_API_KEY"},
	"cursor":                {"CURSOR_ACCESS_TOKEN"},
	"deepseek":              {"DEEPSEEK_API_KEY"},
	"devin":                 {"DEVIN_API_KEY"},
	"firepass":              {"FIREPASS_API_KEY"},
	"fireworks":             {"FIREWORKS_API_KEY"},
	"github-copilot":        {"COPILOT_GITHUB_TOKEN"},
	"gitlab-duo":            {"GITLAB_TOKEN"},
	"gitlab-duo-agent":      {"GITLAB_TOKEN"},
	"gmi-cloud":             {"GMI_API_KEY"},
	"google":                {"GEMINI_API_KEY"},
	"google-antigravity":    {"GROQ_API_KEY"},
	"huggingface":           {"HUGGINGFACE_HUB_TOKEN", "HF_TOKEN"},
	"kilo":                  {"KILO_API_KEY"},
	"kimi-code":             {"KIMI_API_KEY"},
	"litellm":               {"LITELLM_API_KEY"},
	"lm-studio":             {"LM_STUDIO_API_KEY"},
	"meta":                  {"MODEL_API_KEY", "META_API_KEY"},
	"minimax":               {"MINIMAX_API_KEY"},
	"minimax-code":          {"MINIMAX_CODE_API_KEY"},
	"minimax-code-cn":       {"MINIMAX_CODE_CN_API_KEY"},
	"mistral":               {"MISTRAL_API_KEY"},
	"moonshot":              {"MOONSHOT_API_KEY", "KIMI_API_KEY"},
	"nanogpt":               {"NANO_GPT_API_KEY"},
	"nvidia":                {"NVIDIA_API_KEY"},
	"novita":                {"NOVITA_API_KEY"},
	"ollama":                {"OLLAMA_API_KEY"},
	"ollama-cloud":          {"OLLAMA_CLOUD_API_KEY"},
	"openai":                {"OPENAI_API_KEY"},
	"openai-codex":          {"OPENAI_CODEX_OAUTH_TOKEN"},
	"opencode-go":           {"OPENCODE_API_KEY"},
	"opencode-zen":          {"OPENCODE_API_KEY"},
	"openrouter":            {"OPENROUTER_API_KEY"},
	"qianfan":               {"QIANFAN_API_KEY"},
	"qwen-portal":           {"QWEN_OAUTH_TOKEN", "QWEN_PORTAL_API_KEY"},
	"sakana":                {"SAKANA_API_KEY", "FUGU_API_KEY"},
	"siliconflow":           {"SILICONFLOW_API_KEY"},
	"siliconflow-cn":        {"SILICONFLOW_CN_API_KEY"},
	"synthetic":             {"SYNTHETIC_API_KEY"},
	"together":              {"TOGETHER_API_KEY"},
	"umans":                 {"UMANS_AI_CODING_PLAN_API_KEY"},
	"venice":                {"VENICE_API_KEY"},
	"vercel-ai-gateway":     {"AI_GATEWAY_API_KEY"},
	"vllm":                  {"VLLM_API_KEY"},
	"wafer-serverless":      {"WAFER_SERVERLESS_API_KEY"},
	"xai":                   {"XAI_API_KEY"},
	"xai-oauth":             {"XAI_OAUTH_TOKEN", "XAI_API_KEY"},
	"xiaomi":                {"XIAOMI_API_KEY"},
	"xiaomi-token-plan-ams": {"XIAOMI_TOKEN_PLAN_AMS_API_KEY"},
	"xiaomi-token-plan-cn":  {"XIAOMI_TOKEN_PLAN_CN_API_KEY"},
	"xiaomi-token-plan-sgp": {"XIAOMI_TOKEN_PLAN_SGP_API_KEY"},
	"zai":                   {"ZAI_API_KEY"},
	"zenmux":                {"ZENMUX_API_KEY"},
	"zhipu-coding-plan":     {"ZHIPU_API_KEY"},
}
