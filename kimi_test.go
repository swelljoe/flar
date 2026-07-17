package main

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKimiPromptMode verifies detection of non-interactive prompt flags, for
// which kimi rejects --yolo/--auto.
func TestKimiPromptMode(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-c"}, false},
		{[]string{"--session", "session_x"}, false},
		{[]string{"-p", "hi"}, true},
		{[]string{"--prompt", "hi"}, true},
		{[]string{"--prompt=hi"}, true},
		{[]string{"-c", "-p", "hi"}, true},
	}
	for _, c := range cases {
		if got := kimiPromptMode(c.args); got != c.want {
			t.Errorf("kimiPromptMode(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestKimiConfigIsolation verifies that only the global config is copied from
// ~/.kimi-code, and that session state (sessions/, session_index.jsonl,
// user-history/, workspaces.json), live OAuth state (credentials/, oauth/),
// global logs/telemetry, and the self-managed bin/ and updates/ directories
// are left behind. The current project's sessions are supplied at run time by
// the shadow home, and the OAuth dirs are live-bound from the host.
func TestKimiConfigIsolation(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), ".kimi-code")

	// Files that must be copied.
	writeFile(t, filepath.Join(src, "config.toml"), "# kimi config")
	writeFile(t, filepath.Join(src, "tui.toml"), "# tui config")
	writeFile(t, filepath.Join(src, "device_id"), "device-uuid")
	writeFile(t, filepath.Join(src, "credentials", "kimi-code.json"), `{"access_token":"tok"}`)
	writeFile(t, filepath.Join(src, "oauth", "refresh-token"), "refresh")

	// Cross-project session data that must NOT leak.
	writeFile(t, filepath.Join(src, "sessions", "wd_a", "session_a1", "state.json"), `{"workDir":"/a"}`)
	writeFile(t, filepath.Join(src, "session_index.jsonl"), "{}\n")
	writeFile(t, filepath.Join(src, "user-history", "h.jsonl"), "{}\n")
	writeFile(t, filepath.Join(src, "workspaces.json"), `{}`)
	writeFile(t, filepath.Join(src, "logs", "kimi-code.log"), "log")
	writeFile(t, filepath.Join(src, "telemetry", "events.jsonl"), "{}\n")
	writeFile(t, filepath.Join(src, "bin", "kimi"), "binary")
	writeFile(t, filepath.Join(src, "updates", "latest.json"), `{}`)

	// PrepareConfigDir needs the full agent switch; test the copy directly.
	if err := CopyDirExcept(src, dst, kimiSkipCopy); err != nil {
		t.Fatalf("CopyDirExcept: %v", err)
	}

	// Config files must exist.
	mustExist(t, dst, "config.toml")
	mustExist(t, dst, "tui.toml")
	mustExist(t, dst, "device_id")

	// Session state, OAuth state, logs and the self-managed binary must NOT exist.
	mustAbsent(t, dst, "sessions")
	mustAbsent(t, dst, "session_index.jsonl")
	mustAbsent(t, dst, "user-history")
	mustAbsent(t, dst, "workspaces.json")
	mustAbsent(t, dst, "credentials")
	mustAbsent(t, dst, "oauth")
	mustAbsent(t, dst, "logs")
	mustAbsent(t, dst, "telemetry")
	mustAbsent(t, dst, "bin")
	mustAbsent(t, dst, "updates")
}

// kimiTestHome builds a fake host ~/.kimi-code with sessions for two projects.
// projA gets two sessions (a1 with an index line, a2 with an index line, c1
// without state.json attributed only via the index) and projB gets one.
func kimiTestHome(t *testing.T, projA, projB string) (hostHome, hostKimi string) {
	t.Helper()
	hostHome = t.TempDir()
	hostKimi = filepath.Join(hostHome, ".kimi-code")

	writeSession := func(ws, id, workDir, wire string) {
		dir := filepath.Join(hostKimi, "sessions", ws, id)
		if workDir != "" {
			writeFile(t, filepath.Join(dir, "state.json"), fmt.Sprintf(`{"workDir":%q}`, workDir))
		}
		writeFile(t, filepath.Join(dir, "agents", "main", "wire.jsonl"), wire)
	}
	writeSession("wd_a", "session_a1", projA, `{"a":1}`)
	writeSession("wd_a", "session_a2", projA, `{"a":2}`)
	writeSession("wd_c", "session_c1", "", `{"c":1}`) // no state.json; index-only attribution
	writeSession("wd_b", "session_b1", projB, `{"b":1}`)

	indexLine := func(id, ws, workDir string) string {
		return fmt.Sprintf(`{"sessionId":%q,"sessionDir":%q,"workDir":%q}`,
			id, filepath.Join(hostKimi, "sessions", ws, id), workDir)
	}
	writeFile(t, filepath.Join(hostKimi, "session_index.jsonl"), strings.Join([]string{
		indexLine("session_a1", "wd_a", projA),
		indexLine("session_a2", "wd_a", projA),
		indexLine("session_c1", "wd_c", projA),
		indexLine("session_b1", "wd_b", projB),
		`{"sessionId":"session_evil","sessionDir":"/etc","workDir":` + fmt.Sprintf("%q", projA) + `}`,
		"not json at all",
	}, "\n")+"\n")

	histA := fmt.Sprintf("%x.jsonl", md5.Sum([]byte(projA)))
	histB := fmt.Sprintf("%x.jsonl", md5.Sum([]byte(projB)))
	writeFile(t, filepath.Join(hostKimi, "user-history", histA), `{"content":"prompt a"}`+"\n")
	writeFile(t, filepath.Join(hostKimi, "user-history", histB), `{"content":"prompt b"}`+"\n")

	writeFile(t, filepath.Join(hostKimi, "workspaces.json"), fmt.Sprintf(`{
		"version": 1,
		"workspaces": {
			"wd_a": {"root": %q, "name": "a", "created_at": "t", "last_opened_at": "t"},
			"wd_b": {"root": %q, "name": "b", "created_at": "t", "last_opened_at": "t"}
		},
		"deleted_workspace_ids": []
	}`, projA, projB))

	return hostHome, hostKimi
}

// TestPrepareKimiStoreSeedsOnlyCurrentProject verifies that the shadow home is
// seeded with exactly the sessions, index lines, prompt history and workspace
// entries attributed to the current project — nothing from any other project.
func TestPrepareKimiStoreSeedsOnlyCurrentProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	projA := "/home/joe/src/flar"
	projB := "/home/joe/src/other"
	hostHome, hostKimi := kimiTestHome(t, projA, projB)

	// configSrc simulates the per-run temp config copy.
	configSrc := t.TempDir()
	writeFile(t, filepath.Join(configSrc, "config.toml"), "# kimi config")
	writeFile(t, filepath.Join(configSrc, "credentials", "kimi-code.json"), `{"access_token":"tok"}`)
	writeFile(t, filepath.Join(configSrc, "oauth", "refresh-token"), "refresh")

	store, err := prepareKimiStore(hostHome, projA, configSrc)
	if err != nil {
		t.Fatalf("prepareKimiStore: %v", err)
	}

	// Config is seeded from configSrc.
	mustExist(t, store, "config.toml")
	mustExist(t, store, "credentials")
	mustExist(t, store, "oauth")
	mustAbsent(t, store, filepath.Join("credentials", "kimi-code.json"))
	mustAbsent(t, store, filepath.Join("oauth", "refresh-token"))
	mustExist(t, store, ".seeded")

	// This project's sessions are seeded, including the index-only one; the
	// other project's and the out-of-tree one are not.
	mustExist(t, store, filepath.Join("sessions", "wd_a", "session_a1", "state.json"))
	mustExist(t, store, filepath.Join("sessions", "wd_a", "session_a1", "agents", "main", "wire.jsonl"))
	mustExist(t, store, filepath.Join("sessions", "wd_a", "session_a2", "state.json"))
	mustExist(t, store, filepath.Join("sessions", "wd_c", "session_c1", "agents", "main", "wire.jsonl"))
	mustAbsent(t, store, filepath.Join("sessions", "wd_b"))
	mustAbsent(t, store, filepath.Join("sessions", "etc"))

	// The scoped index holds only this project's sessions, in sorted order,
	// with verbatim host lines where they existed.
	data, err := os.ReadFile(filepath.Join(store, "session_index.jsonl"))
	if err != nil {
		t.Fatalf("read scoped index: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("scoped index has %d lines, want 3: %q", len(lines), string(data))
	}
	wantIDs := []string{"session_a1", "session_a2", "session_c1"}
	for i, wantID := range wantIDs {
		var e kimiSessionIndexEntry
		if err := json.Unmarshal([]byte(lines[i]), &e); err != nil {
			t.Fatalf("index line %d not parseable: %v", i, err)
		}
		if e.SessionID != wantID {
			t.Errorf("index line %d id = %q, want %q", i, e.SessionID, wantID)
		}
		if e.WorkDir != projA {
			t.Errorf("index line %d workDir = %q, want %q", i, e.WorkDir, projA)
		}
		if !strings.HasPrefix(e.SessionDir, filepath.Join(hostKimi, "sessions")+string(filepath.Separator)) {
			t.Errorf("index line %d sessionDir %q escapes sessions/", i, e.SessionDir)
		}
	}
	if strings.Contains(string(data), "session_b1") || strings.Contains(string(data), "session_evil") {
		t.Errorf("scoped index leaks other projects' or out-of-tree sessions: %q", string(data))
	}

	// Prompt history is scoped to this project's md5 file.
	mustExist(t, store, filepath.Join("user-history", fmt.Sprintf("%x.jsonl", md5.Sum([]byte(projA)))))
	mustAbsent(t, store, filepath.Join("user-history", fmt.Sprintf("%x.jsonl", md5.Sum([]byte(projB)))))

	// workspaces.json is filtered to this project.
	wsData, err := os.ReadFile(filepath.Join(store, "workspaces.json"))
	if err != nil {
		t.Fatalf("read workspaces.json: %v", err)
	}
	var ws struct {
		Version    int                          `json:"version"`
		Workspaces map[string]map[string]string `json:"workspaces"`
	}
	if err := json.Unmarshal(wsData, &ws); err != nil {
		t.Fatalf("workspaces.json not parseable: %v", err)
	}
	if len(ws.Workspaces) != 1 || ws.Workspaces["wd_a"]["root"] != projA {
		t.Errorf("workspaces.json = %q, want only wd_a for %q", string(wsData), projA)
	}
}

// TestPrepareKimiStoreSeedsOnce verifies the fork semantics: after the
// one-time seed, later host-side sessions are not pulled in and store contents
// are not clobbered.
func TestPrepareKimiStoreSeedsOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	projA := "/home/joe/src/flar"
	projB := "/home/joe/src/other"
	hostHome, hostKimi := kimiTestHome(t, projA, projB)
	configSrc := t.TempDir()

	store, err := prepareKimiStore(hostHome, projA, configSrc)
	if err != nil {
		t.Fatalf("prepareKimiStore: %v", err)
	}

	// A session created inside the sandbox (attacker-controlled from flar's
	// perspective) must survive; a new host-side session must NOT be pulled in.
	writeFile(t, filepath.Join(store, "session_index.jsonl"), "sandboxed-line\n")
	writeFile(t, filepath.Join(hostKimi, "sessions", "wd_a", "session_a3", "state.json"), fmt.Sprintf(`{"workDir":%q}`, projA))

	store2, err := prepareKimiStore(hostHome, projA, configSrc)
	if err != nil {
		t.Fatalf("prepareKimiStore (second): %v", err)
	}
	if store2 != store {
		t.Errorf("store path changed between runs: %q vs %q", store2, store)
	}
	data, err := os.ReadFile(filepath.Join(store2, "session_index.jsonl"))
	if err != nil {
		t.Fatalf("read scoped index: %v", err)
	}
	if string(data) != "sandboxed-line\n" {
		t.Errorf("scoped index was re-seeded: %q", string(data))
	}
	mustAbsent(t, store2, filepath.Join("sessions", "wd_a", "session_a3"))
}

// TestPrepareKimiStoreFreshHost verifies seeding against a host with no Kimi
// history at all: the store still gets the config plus the empty session
// structures kimi expects.
func TestPrepareKimiStoreFreshHost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	hostHome := t.TempDir()
	configSrc := t.TempDir()
	writeFile(t, filepath.Join(configSrc, "config.toml"), "# kimi config")

	store, err := prepareKimiStore(hostHome, "/home/joe/src/flar", configSrc)
	if err != nil {
		t.Fatalf("prepareKimiStore: %v", err)
	}
	mustExist(t, store, "config.toml")
	mustExist(t, store, "credentials")
	mustExist(t, store, "oauth")
	mustExist(t, store, "sessions")
	mustExist(t, store, "session_index.jsonl")
	mustExist(t, store, "user-history")
	mustAbsent(t, store, "workspaces.json")
}
