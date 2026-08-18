package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initOmpTestHistoryDB creates the schema of omp's centralized prompt-history
// database exactly as history-storage.ts defines it: the history table, an index,
// the FTS5 virtual table over it, and the AFTER INSERT trigger that keeps it in
// sync. This lets seedOmpHistory's FTS rebuild be exercised against a real index.
func initOmpTestHistoryDB(t *testing.T, path string) {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatalf("open test history db: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			prompt TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
			cwd TEXT,
			session_id TEXT
		);
		CREATE INDEX idx_history_created_at ON history(created_at DESC);
		CREATE VIRTUAL TABLE history_fts USING fts5(prompt, content='history', content_rowid='id');
		CREATE TRIGGER history_ai AFTER INSERT ON history BEGIN
			INSERT INTO history_fts(rowid, prompt) VALUES (new.id, new.prompt);
		END;
	`)
	if err != nil {
		t.Fatalf("create history schema: %v", err)
	}
}

// initOmpTestAgentDB creates a minimal omp agent.db with just the auth_credentials
// table — enough for seedOmpAuth's provider narrowing to be exercised.
func initOmpTestAgentDB(t *testing.T, path string) {
	t.Helper()
	db, err := openSQLite(path)
	if err != nil {
		t.Fatalf("open test agent db: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE auth_credentials (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL,
		credential_type TEXT NOT NULL,
		identity_key TEXT,
		data TEXT,
		disabled_cause TEXT
	)`)
	if err != nil {
		t.Fatalf("create agent db schema: %v", err)
	}
}

// TestEncodeOmpSessionSlug verifies the slug port matches omp's on-disk directory
// naming for all three scope branches plus the same-path edge. Expected values are
// taken from real ~/.omp/agent/sessions entries on the dev host, which makes this a
// byte-exactness check: a mismatch here means session resume breaks.
func TestEncodeOmpSessionSlug(t *testing.T) {
	t.Setenv("HOME", "/home/joe")
	home, _ := os.UserHomeDir()

	cases := []struct{ cwd, want string }{
		// home-relative scope (the common case): "-<homeRelative with / -> -> -"
		{"/home/joe/src/flar", "-src-flar"},
		{"/home/joe/src/laguna-experiments", "-src-laguna-experiments"},
		{"/home/joe/work/.flar-test/dots_and-spaces", "-work-.flar-test-dots_and-spaces"},
		// home itself collapses to the bare prefix (Node's path.relative is "" here,
		// not ".", so this guards the Go "." -> "" normalization).
		{home, "-"},
		// temp-relative scope: "-tmp-<tempRelative>"
		{filepath.Join(os.TempDir(), "omp-decoy"), "-tmp-omp-decoy"},
		// absolute scope (neither under home nor tmp): "--<abs without leading />--"
		{"/var/www/site", "--var-www-site--"},
	}
	for _, tc := range cases {
		if got := encodeOmpSessionSlug(tc.cwd); got != tc.want {
			t.Errorf("encodeOmpSessionSlug(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
}

// TestSeedOmpHistoryScopesToProject verifies the per-project fork: only this
// project's prompts remain, other projects' and unattributed (NULL cwd) rows are
// removed, and the FTS5 index is rebuilt to match (a MATCH query returns exactly
// this project's prompts — proving the old cross-project index entries are gone).
func TestSeedOmpHistoryScopesToProject(t *testing.T) {
	host := t.TempDir()
	src := filepath.Join(host, "history.db")
	initOmpTestHistoryDB(t, src)

	db, err := openSQLite(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO history (prompt, cwd) VALUES ('alpha prompt', '/home/joe/src/flar');
		INSERT INTO history (prompt, cwd) VALUES ('beta prompt',  '/home/joe/src/flar');
		INSERT INTO history (prompt, cwd) VALUES ('gamma prompt', '/home/joe/src/other');
		INSERT INTO history (prompt, cwd) VALUES ('delta prompt', NULL);
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	dst := t.TempDir()
	if err := seedOmpHistory(host, dst, "/home/joe/src/flar"); err != nil {
		t.Fatalf("seedOmpHistory: %v", err)
	}

	shadow, err := openSQLite(filepath.Join(dst, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer shadow.Close()

	var count int
	if err := shadow.QueryRow(`SELECT count(*) FROM history`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("history row count = %d, want 2 (only this project)", count)
	}

	// The other project's prompt must be gone.
	var other int
	shadow.QueryRow(`SELECT count(*) FROM history WHERE cwd <> '/home/joe/src/flar'`).Scan(&other)
	if other != 0 {
		t.Errorf("cross-project rows survived: %d", other)
	}

	// FTS must reflect the reduced table: MATCH returns exactly this project's rows.
	// If the old index were stale it would still surface gamma/delta.
	fts, err := shadow.Query(`SELECT h.prompt FROM history_fts f JOIN history h ON h.id = f.rowid WHERE history_fts MATCH 'prompt'`)
	if err != nil {
		t.Fatalf("fts match: %v", err)
	}
	var matched []string
	for fts.Next() {
		var p string
		fts.Scan(&p)
		matched = append(matched, p)
	}
	fts.Close()
	if len(matched) != 2 {
		t.Errorf("fts match returned %d rows (%v), want 2 (this project only)", len(matched), matched)
	}
	for _, m := range matched {
		if !strings.Contains(m, "prompt") || m == "gamma prompt" || m == "delta prompt" {
			t.Errorf("fts surfaced unexpected row: %q", m)
		}
	}
}

// TestSeedOmpAuthNarrowsToProvider verifies the agent.db fork keeps only the pinned
// provider's credentials and drops every other provider's keys.
func TestSeedOmpAuthNarrowsToProvider(t *testing.T) {
	host := t.TempDir()
	src := filepath.Join(host, "agent.db")
	initOmpTestAgentDB(t, src)

	db, err := openSQLite(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO auth_credentials (provider, credential_type, identity_key, data) VALUES
			('anthropic', 'api_key', 'user', '{"key":"ANTHROPIC_SECRET"}'),
			('openai',    'api_key', 'user', '{"key":"OPENAI_SECRET"}'),
			('moonshot',  'api_key', 'user', '{"key":"MOONSHOT_SECRET"}');
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	dst := t.TempDir()
	if err := seedOmpAuth(host, dst, "anthropic"); err != nil {
		t.Fatalf("seedOmpAuth: %v", err)
	}

	shadow, err := openSQLite(filepath.Join(dst, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer shadow.Close()

	var count int
	if err := shadow.QueryRow(`SELECT count(*) FROM auth_credentials`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("auth_credentials count = %d, want 1 (anthropic only)", count)
	}

	var providers string
	shadow.QueryRow(`SELECT group_concat(provider) FROM auth_credentials`).Scan(&providers)
	if providers != "anthropic" {
		t.Errorf("surviving providers = %q, want anthropic", providers)
	}
}

// TestSeedOmpAuthUnknownProviderDropsAll verifies that pinning a provider with no
// stored credentials empties the credential table — the safe outcome (no leak).
func TestSeedOmpAuthUnknownProviderDropsAll(t *testing.T) {
	host := t.TempDir()
	src := filepath.Join(host, "agent.db")
	initOmpTestAgentDB(t, src)

	db, err := openSQLite(src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO auth_credentials (provider, credential_type, data) VALUES ('openai','api_key','{}')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := seedOmpAuth(host, dst, "does-not-exist"); err != nil {
		t.Fatalf("seedOmpAuth: %v", err)
	}
	shadow, err := openSQLite(filepath.Join(dst, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer shadow.Close()
	var count int
	shadow.QueryRow(`SELECT count(*) FROM auth_credentials`).Scan(&count)
	if count != 0 {
		t.Errorf("credentials survived unknown-provider narrowing: %d", count)
	}
}

// TestPrepareOmpStore verifies the end-to-end seed: a project-scoped shadow store is
// created under flar's state dir, history.db is forked to this project only, agent.db
// keeps all credentials when no model is pinned (and narrows when one is), an empty
// sessions/ root exists for the live-bind, and the static config overlay excludes the
// centralized entries. A second prepare does not re-copy host data.
func TestPrepareOmpStore(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	hostHome := t.TempDir()

	agent := filepath.Join(hostHome, ".omp", "agent")
	if err := os.MkdirAll(filepath.Join(agent, "sessions", "-src-flar"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(agent, "config.yml"), "model: x\n")
	writeFile(t, filepath.Join(agent, "models.yml"), "providers: []\n")
	// Cross-project session that must never enter the store (only this project's slug
	// is live-bound at run time).
	writeFile(t, filepath.Join(agent, "sessions", "-other-proj", "s.jsonl"), `{"role":"user"}`)

	initOmpTestHistoryDB(t, filepath.Join(agent, "history.db"))
	hdb, err := openSQLite(filepath.Join(agent, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = hdb.Exec(`
		INSERT INTO history (prompt, cwd) VALUES ('a', '/home/joe/src/flar');
		INSERT INTO history (prompt, cwd) VALUES ('b', '/home/joe/src/other');
	`)
	hdb.Close()

	initOmpTestAgentDB(t, filepath.Join(agent, "agent.db"))
	ad, _ := openSQLite(filepath.Join(agent, "agent.db"))
	_, _ = ad.Exec(`INSERT INTO auth_credentials (provider, credential_type, data) VALUES ('anthropic','api_key','a'),('openai','api_key','b')`)
	ad.Close()

	const projA = "/home/joe/src/flar"

	// No model pinned: full agent store is snapshot-copied.
	store, err := prepareOmpStore(hostHome, projA, "")
	if err != nil {
		t.Fatalf("prepareOmpStore: %v", err)
	}
	wantStore := filepath.Join(state, "flar", "omp", claudeProjectSlug(projA))
	if store != wantStore {
		t.Errorf("store path = %q, want %q", store, wantStore)
	}

	mustExist(t, store, ".seeded")
	mustExist(t, store, "config.yml")    // static config copied
	mustAbsent(t, store, "agent.db-wal") // sidecars skipped
	mustAbsent(t, store, filepath.Join("sessions", "-other-proj"))

	// history.db forked to this project only.
	hist, err := openSQLite(filepath.Join(store, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	var hcount int
	if err := hist.QueryRow(`SELECT count(*) FROM history`).Scan(&hcount); err != nil {
		t.Fatal(err)
	}
	hist.Close()
	if hcount != 1 {
		t.Errorf("store history count = %d, want 1", hcount)
	}

	// agent.db keeps all providers when no model pinned.
	auth, err := openSQLite(filepath.Join(store, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	var acount int
	if err := auth.QueryRow(`SELECT count(*) FROM auth_credentials`).Scan(&acount); err != nil {
		t.Fatal(err)
	}
	auth.Close()
	if acount != 2 {
		t.Errorf("store auth credentials = %d, want 2 (unpinned keeps all)", acount)
	}

	// sessions/ root exists for the live-bind but does not leak other projects.
	mustExist(t, store, filepath.Join("sessions"))

	// Seed-once: a new host history row must NOT be pulled in on second prepare.
	hdb2, _ := openSQLite(filepath.Join(agent, "history.db"))
	_, _ = hdb2.Exec(`INSERT INTO history (prompt, cwd) VALUES ('c', '/home/joe/src/flar')`)
	hdb2.Close()

	store2, err := prepareOmpStore(hostHome, projA, "")
	if err != nil || store2 != store {
		t.Fatalf("second prepare = %q, %v; want %q", store2, err, store)
	}
	hist3, _ := openSQLite(filepath.Join(store, "history.db"))
	var hcount3 int
	hist3.QueryRow(`SELECT count(*) FROM history`).Scan(&hcount3)
	hist3.Close()
	if hcount3 != 1 {
		t.Errorf("post-second-prepare history count = %d, want 1 (seed-once)", hcount3)
	}
}

// TestPrepareOmpStoreModelNarrowsAuth verifies that pinning a model narrows the
// shadow agent.db to just that provider's credentials at seed time.
func TestPrepareOmpStoreModelNarrowsAuth(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	hostHome := t.TempDir()

	agent := filepath.Join(hostHome, ".omp", "agent")
	os.MkdirAll(agent, 0o700)
	initOmpTestAgentDB(t, filepath.Join(agent, "agent.db"))
	ad, _ := openSQLite(filepath.Join(agent, "agent.db"))
	_, _ = ad.Exec(`INSERT INTO auth_credentials (provider, credential_type, data) VALUES
		('anthropic','api_key','a'),('openai','api_key','b')`)
	ad.Close()

	store, err := prepareOmpStore(hostHome, "/home/joe/src/flar", "anthropic/claude-sonnet")
	if err != nil {
		t.Fatalf("prepareOmpStore: %v", err)
	}
	auth, err := openSQLite(filepath.Join(store, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	var providers string
	auth.QueryRow(`SELECT group_concat(provider) FROM auth_credentials`).Scan(&providers)
	if providers != "anthropic" {
		t.Errorf("providers after model pin = %q, want anthropic", providers)
	}
}

// TestOmpProviderEnvVars verifies the provider -> env-var mapping used for the
// pinned-model narrowing path, including multi-value providers and the no-pin /
// unknown-provider edges.
func TestOmpProviderEnvVars(t *testing.T) {
	if got := ompAgentEnvVars(""); len(got) != 0 {
		t.Errorf("no model -> env vars = %v, want none", got)
	}
	if got := ompAgentEnvVars("anthropic/claude-sonnet"); !eqSlice(got, []string{"ANTHROPIC_API_KEY"}) {
		t.Errorf("anthropic env = %v", got)
	}
	if got := ompAgentEnvVars("moonshot/moonshot-v1"); !eqSlice(got, []string{"MOONSHOT_API_KEY", "KIMI_API_KEY"}) {
		t.Errorf("moonshot env = %v", got)
	}
	// unknown provider: no known vars (a warning is printed; the slice is empty).
	if got := ompAgentEnvVars("not-a-real-provider/x"); len(got) != 0 {
		t.Errorf("unknown provider env = %v, want none", got)
	}
}

// TestProviderFromOmpModel checks model-id -> provider extraction and edges.
func TestProviderFromOmpModel(t *testing.T) {
	cases := map[string]string{
		"anthropic/claude-sonnet":       "anthropic",
		"xiaomi-token-plan-sgp/mimo-v2": "xiaomi-token-plan-sgp",
		"openai/gpt-5:1":                "openai",
		"":                              "",
		"noslash":                       "",
	}
	for in, want := range cases {
		if got := providerFromOmpModel(in); got != want {
			t.Errorf("providerFromOmpModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPrepareOmpStoreConfigIsolation verifies the static overlay copy (the
// ompSkipCopy gate) excludes centralized state — history.db*, agent.db*, models.db*
// and sessions — while keeping plain config files. This is what PrepareConfigDir's
// AgentOmp case relies on if it were ever re-introduced, and documents that those
// entries are run-time concerns handled by prepareOmpStore.
func TestPrepareOmpStoreConfigIsolation(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "config.yml"), "model: x\n")
	writeFile(t, filepath.Join(src, "models.yml"), "providers: []\n")
	// Centralized / cross-project entries that must be excluded from a static copy.
	writeFile(t, filepath.Join(src, "history.db"), "fake")
	writeFile(t, filepath.Join(src, "history.db-wal"), "w")
	writeFile(t, filepath.Join(src, "agent.db"), "fake")
	writeFile(t, filepath.Join(src, "models.db"), "m")
	writeFile(t, filepath.Join(src, "sessions", "-other", "s.jsonl"), `{"x":1}`)

	dst := filepath.Join(t.TempDir(), ".omp-agent")
	if err := CopyDirExcept(src, dst, ompSkipCopy); err != nil {
		t.Fatalf("CopyDirExcept: %v", err)
	}

	mustExist(t, dst, "config.yml")
	mustExist(t, dst, "models.yml")
	mustAbsent(t, dst, "history.db")
	mustAbsent(t, dst, "history.db-wal")
	mustAbsent(t, dst, "agent.db")
	mustAbsent(t, dst, "models.db")
	mustAbsent(t, dst, "sessions")
}

// TestOmpAgentDir verifies the host agent-dir resolution and the PI_CONFIG_DIR
// override.
func TestOmpAgentDir(t *testing.T) {
	t.Setenv("PI_CONFIG_DIR", "")
	if got := ompAgentDir("/home/joe"); got != "/home/joe/.omp/agent" {
		t.Errorf("default agent dir = %q, want /home/joe/.omp/agent", got)
	}
	t.Setenv("PI_CONFIG_DIR", "/custom/config")
	if got := ompAgentDir("/home/joe"); got != "/custom/config/agent" {
		t.Errorf("override agent dir = %q, want /custom/config/agent", got)
	}
}

func eqSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
