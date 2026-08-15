package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// writeOmpSessionFile writes a minimal omp session transcript whose header
// record self-attests the given cwd. The first line is noise to prove the
// attestation scan does not stop at the first JSON record.
func writeOmpSessionFile(t *testing.T, dir, name, cwd string) {
	t.Helper()
	body := `{"type":"session","version":3,"cwd":` + jsonQuote(cwd) + `}` + "\n"
	writeFile(t, filepath.Join(dir, name), `{"type":"meta"}`+"\n"+body)
}

func jsonQuote(s string) string {
	return `"` + s + `"`
}

func initOmpHistoryDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			prompt TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
			cwd TEXT,
			session_id TEXT
		)`,
		`CREATE VIRTUAL TABLE history_fts USING fts5(prompt, content='history', content_rowid='id')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	return db
}

func initOmpAgentDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE auth_credentials (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			provider TEXT NOT NULL,
			credential_type TEXT,
			data TEXT,
			disabled_cause TEXT,
			identity_key TEXT,
			created_at TEXT,
			updated_at TEXT
		)`,
		`CREATE TABLE auth_credential_refresh_leases (
			credential_id INTEGER PRIMARY KEY,
			owner TEXT, expires_at_ms INTEGER, updated_at TEXT
		)`,
		`CREATE TABLE auth_credential_blocks (
			credential_id INTEGER, provider_key TEXT, block_scope TEXT,
			blocked_until_ms INTEGER, updated_at TEXT
		)`,
		`CREATE TABLE auth_credential_block_mirror_guard (credential_id INTEGER PRIMARY KEY)`,
		`CREATE TABLE cache (key TEXT PRIMARY KEY, value TEXT, expires_at INTEGER)`,
		`CREATE TABLE clients (install_id TEXT PRIMARY KEY, hostname TEXT, first_seen TEXT, last_seen TEXT)`,
		`CREATE TABLE client_usage (id INTEGER PRIMARY KEY AUTOINCREMENT, recorded_at TEXT, install_id TEXT, provider TEXT)`,
		`CREATE TABLE usage_cost_history (id INTEGER PRIMARY KEY AUTOINCREMENT, recorded_at TEXT, provider TEXT)`,
		`CREATE TABLE usage_history (id INTEGER PRIMARY KEY AUTOINCREMENT, recorded_at TEXT, provider TEXT)`,
		`CREATE TABLE model_perf (model_key TEXT PRIMARY KEY, samples INTEGER)`,
		`CREATE TABLE model_usage (model_key TEXT PRIMARY KEY, last_used_at TEXT)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	return db
}

func TestOmpSessionBucket(t *testing.T) {
	if os.TempDir() == "/" {
		t.Skip("TMPDIR=/ makes the tmp root match everything")
	}
	fakeHome := t.TempDir() // under os.TempDir(); home check must win by precedence
	mk := func(root, rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name            string
		home, dir, want string
	}{
		// Home-relative; cwd is also under the tmp root, but home wins.
		{"home relative", fakeHome, mk(fakeHome, "src/flar"), "-src-flar"},
		{"dots and spaces preserved", fakeHome, mk(fakeHome, "src/.flar-test/dots_and-spaces"), "-src-.flar-test-dots_and-spaces"},
		{"cwd is home itself", fakeHome, fakeHome, "-"},
		// Temp-root relative: os.TempDir() is the real temp dir.
		{"tmp root relative", "/nonexistent-home", mk(os.TempDir(), "sub/proj"), "-tmp-sub-proj"},
		{"tmp root is cwd itself", "/nonexistent-home", os.TempDir(), "-tmp"},
	}
	for _, c := range cases {
		got := ompSessionBucket(c.home, c.dir)
		if got != c.want {
			t.Errorf("%s: ompSessionBucket(%q, %q) = %q, want %q", c.name, c.home, c.dir, got, c.want)
		}
	}
	// Absolute fallback for a path under neither root. /usr always exists and
	// is never below the temp or home roots on a real system.
	if got := ompSessionBucket(fakeHome, "/usr/share"); got != "--usr-share--" {
		t.Errorf("absolute bucket = %q, want --usr-share--", got)
	}
}

func TestResolveOmpProviders(t *testing.T) {
	cases := []struct {
		model string
		want  []string
	}{
		{"", nil},
		{"  ", nil},
		{"deepseek", []string{"deepseek"}},
		{"DeepSeek/some-model-v2", []string{"deepseek"}},
		{"llama.cpp-cat/unsloth/Qwen3.8-27B-GGUF:UD-Q8_K_XL", []string{"llama.cpp-cat"}},
		{"openai/gpt-5.2", []string{"openai"}},
	}
	for _, c := range cases {
		got := resolveOmpProviders(c.model)
		if len(got) != len(c.want) {
			t.Errorf("resolveOmpProviders(%q) = %v, want %v", c.model, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("resolveOmpProviders(%q)[%d] = %q, want %q", c.model, i, got[i], c.want[i])
			}
		}
	}
}

func TestResolvedOmpEnvVarsScoped(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-ds")
	t.Setenv("ANTHROPIC_API_KEY", "sk-anthropic")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("PI_SMOL_MODEL", "foo/bar")
	t.Setenv("PI_PROXY_PORT", "3128")
	t.Setenv("PI_OAUTH_CLIENT_ID", "x")
	t.Setenv("OMP_PROFILE", "work")

	out, scoped := resolvedOmpEnvVars(resolveOmpProviders("deepseek"))
	if !scoped {
		t.Fatal("resolvedOmpEnvVars(scoped) = not scoped")
	}
	if out["DEEPSEEK_API_KEY"] != "sk-ds" {
		t.Errorf("scoped env missing DEEPSEEK_API_KEY: %v", out)
	}
	for _, leaked := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if _, ok := out[leaked]; ok {
			t.Errorf("scoped env leaks %s: %v", leaked, out)
		}
	}
	// Model/role configuration prefixes are always forwarded.
	if out["PI_SMOL_MODEL"] != "foo/bar" || out["OMP_PROFILE"] != "work" {
		t.Errorf("config-prefix vars not forwarded: %v", out)
	}
	// Proxy and OAuth plumbing must never be forwarded.
	for _, banned := range []string{"PI_PROXY_PORT", "PI_OAUTH_CLIENT_ID"} {
		if _, ok := out[banned]; ok {
			t.Errorf("banned var %s forwarded: %v", banned, out)
		}
	}
}

func TestResolvedOmpEnvVarsUnscoped(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-ds")
	t.Setenv("ANTHROPIC_API_KEY", "sk-anthropic")

	out, scoped := resolvedOmpEnvVars(nil)
	if scoped {
		t.Fatal("resolvedOmpEnvVars(nil) reported scoped")
	}
	if _, ok := out["DEEPSEEK_API_KEY"]; !ok {
		t.Errorf("unscoped env missing DEEPSEEK_API_KEY: %v", out)
	}
	if _, ok := out["ANTHROPIC_API_KEY"]; !ok {
		t.Errorf("unscoped env missing ANTHROPIC_API_KEY: %v", out)
	}
}

// TestPrepareOmpStoreSeedsOnce verifies the full shadow-store seed: session
// buckets filtered by internal cwd attestation, a project-filtered history.db
// fork with rebuilt FTS index, an agent.db fork scoped to one provider, config
// file merge, and one-time seeding (no re-merge after first use).
func TestPrepareOmpStoreSeedsOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	projectA := "/work/a"

	hostAgent := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(hostAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	// Two session buckets: one self-attested as this project, one as another
	// project. The host bucket names deliberately differ from the name the
	// encoder computes for each attested cwd (-work-a vs --work-a--), proving
	// seed selection follows the internal cwd, not the directory name. A
	// second file in the right bucket proves whole-bucket copy.
	bucketA := filepath.Join(hostAgent, "sessions", "-work-a")
	writeOmpSessionFile(t, bucketA, "s1.jsonl", projectA)
	writeOmpSessionFile(t, bucketA, "s2.jsonl", projectA)
	bucketB := filepath.Join(hostAgent, "sessions", "-work-b")
	writeOmpSessionFile(t, bucketB, "s3.jsonl", "/work/b")

	// Prompt history mixing projects; FTS must reflect the filter.
	hdb := initOmpHistoryDB(t, filepath.Join(hostAgent, "history.db"))
	if _, err := hdb.Exec(`INSERT INTO history (prompt, cwd) VALUES
		('zebra secret alpha', '/work/b'),
		('project a prompt one', ?),
		('project a prompt two', ?),
		('unscoped row', NULL)`, projectA, projectA); err != nil {
		t.Fatal(err)
	}
	hdb.Close()

	// Credential store with three providers plus telemetry rows.
	adbg := initOmpAgentDB(t, filepath.Join(hostAgent, "agent.db"))
	if _, err := adbg.Exec(`INSERT INTO auth_credentials (provider, credential_type, data) VALUES
		('deepseek', 'api_key', '{"apiKey":"ds"}'),
		('anthropic', 'oauth', '{"token":"anth"}'),
		('moonshot', 'api_key', '{"apiKey":"ms"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := adbg.Exec(`INSERT INTO auth_credential_refresh_leases (credential_id, owner) VALUES (2, 'host')`); err != nil {
		t.Fatal(err)
	}
	if _, err := adbg.Exec(`INSERT INTO cache (key, value) VALUES ('session:sticky:anthropic:abc', '{"credentialId":2}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := adbg.Exec(`INSERT INTO clients (install_id) VALUES ('host-installer')`); err != nil {
		t.Fatal(err)
	}
	adbg.Close()

	// Config files prepared by PrepareConfigDir.
	tempConfig := t.TempDir()
	writeFile(t, filepath.Join(tempConfig, "omp-config", "config.yml"), "modelRoles: {}\n")
	writeFile(t, filepath.Join(tempConfig, "omp-config", "models.yml"), "providers: []\n")

	store, err := prepareOmpStore(home, projectA, resolveOmpProviders("deepseek"), tempConfig)
	if err != nil {
		t.Fatal(err)
	}
	// Sessions: only the attested bucket, re-named by the encoder for its cwd.
	mustExist(t, store, filepath.Join("sessions", "--work-a--", "s1.jsonl"))
	mustExist(t, store, filepath.Join("sessions", "--work-a--", "s2.jsonl"))
	mustAbsent(t, store, filepath.Join("sessions", "-work-b"))

	// History fork: only this project's rows; FTS no longer matches deleted terms.
	db, err := openSQLite(filepath.Join(store, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history WHERE cwd = ?`, projectA).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("forked history rows for project = %d, want 2", count)
	}
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("forked history total rows = %d, want 2 (cwd filter failed)", total)
	}
	var ftsHits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history_fts WHERE history_fts MATCH 'zebra'`).Scan(&ftsHits); err != nil {
		t.Fatalf("FTS query after rebuild: %v", err)
	}
	if ftsHits != 0 {
		t.Errorf("FTS still matches deleted term 'zebra': %d hits (rebuild failed)", ftsHits)
	}
	var aHits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history_fts WHERE history_fts MATCH 'project'`).Scan(&aHits); err != nil {
		t.Fatal(err)
	}
	if aHits != 2 {
		t.Errorf("FTS matches for surviving term = %d, want 2", aHits)
	}
	db.Close()

	// Agent DB fork: credentials scoped to deepseek; foreign rows and all
	// telemetry removed.
	adbgf, err := openSQLite(filepath.Join(store, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	var providers []string
	rows, err := adbgf.Query(`SELECT provider FROM auth_credentials ORDER BY provider`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			providers = append(providers, p)
		}
	}
	rows.Close()
	if len(providers) != 1 || providers[0] != "deepseek" {
		t.Errorf("forked credentials = %v, want [deepseek]", providers)
	}
	for _, table := range []string{
		"auth_credential_refresh_leases",
		"cache",
		"clients",
		"client_usage",
		"usage_cost_history",
		"usage_history",
		"model_perf",
		"model_usage",
	} {
		var n int
		if err := adbgf.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("forked %s rows = %d, want 0", table, n)
		}
	}
	adbgf.Close()

	// Config files merged into the store.
	mustExist(t, store, "config.yml")
	mustExist(t, store, "models.yml")
	mustExist(t, store, ".seeded")

	// One-time seed: host changes after seeding must not reach the shadow.
	writeOmpSessionFile(t, bucketB, "s4.jsonl", "/work/b")
	store2, err := prepareOmpStore(home, projectA, resolveOmpProviders("deepseek"), tempConfig)
	if err != nil {
		t.Fatal(err)
	}
	if store2 != store {
		t.Errorf("second call = %q, want same store %q", store2, store)
	}
	mustAbsent(t, store, filepath.Join("sessions", "-work-b", "s4.jsonl"))
}

// TestPrepareOmpStoreUnscopedKeepsCredentials verifies that without -omp-model
// the credential store is forked whole (all providers kept), so nothing breaks
// at the cost of maximum exposure — the warning is printed by RunSandbox.
func TestPrepareOmpStoreUnscopedKeepsCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")

	hostAgent := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(hostAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	adbg := initOmpAgentDB(t, filepath.Join(hostAgent, "agent.db"))
	if _, err := adbg.Exec(`INSERT INTO auth_credentials (provider, credential_type, data) VALUES
		('deepseek', 'api_key', '{}'), ('anthropic', 'oauth', '{}')`); err != nil {
		t.Fatal(err)
	}
	adbg.Close()

	store, err := prepareOmpStore(home, "/work/x", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := openSQLite(filepath.Join(store, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM auth_credentials`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if n != 2 {
		t.Errorf("unscoped fork kept %d credentials, want 2", n)
	}
}

// TestPrepareOmpStoreMissingAgentDir reports a clear error when omp has never
// been used on the host.
func TestPrepareOmpStoreMissingAgentDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	if _, err := prepareOmpStore(home, "/work/x", nil, t.TempDir()); err == nil {
		t.Fatal("prepareOmpStore with missing ~/.omp/agent = nil error")
	}
}

// TestPrepareOmpStoreFirstRunWithoutHostState verifies seeding works for a
// fresh omp install (no history.db, no agent.db, no sessions) so the shadow
// store is created and marked seeded.
func TestPrepareOmpStoreFirstRunWithoutHostState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")

	hostAgent := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(hostAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(hostAgent, "config.yml"), "modelRoles: {}\n")

	tempConfig := t.TempDir()
	writeFile(t, filepath.Join(tempConfig, "omp-config", "config.yml"), "modelRoles: {}\n")

	store, err := prepareOmpStore(home, "/work/x", resolveOmpProviders("deepseek"), tempConfig)
	if err != nil {
		t.Fatal(err)
	}
	mustExist(t, store, ".seeded")
	mustExist(t, store, "config.yml")
	mustAbsent(t, store, filepath.Join(".omp", "agent")) // no accidental nesting
}
