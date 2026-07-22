package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// poolTestHome builds a fake host state directory (~/.local/state/poolside)
// with sessions and trajectories for two projects. Workspace A gets two
// sessions (a1, a2); workspace B gets one (b1). Per-project prompt history
// and logs are also created for each workspace.
func poolTestHome(t *testing.T, projA, projB string) (hostHome, hostState string) {
	t.Helper()
	hostHome = t.TempDir()
	hostState = filepath.Join(hostHome, ".local", "state", "poolside")

	// Session IDs (standard UUIDs).
	sessA1 := "019f8828-6c7b-7800-8f77-9388a5986887"
	sessA2 := "019f8829-6c7b-7800-8f77-9388a5986888"
	sessB1 := "019f8830-6c7b-7800-8f77-9388a5986889"

	writePoolSession := func(id string) {
		writeFile(t, filepath.Join(hostState, "sessions", "session-"+id+".json"),
			`{"run_id":"`+id+`","timestamp":"2026-01-01T00:00:00Z","agent_id":"","session_id":"`+id+`"}`)
	}

	writePoolTrajectory := func(id, workDir string) {
		start := `{"type":"session.start","session_start":{"working_directories":[`
		if workDir != "" {
			start += `"` + workDir + `"`
		}
		start += `],"prompt":""}}`
		writeFile(t, filepath.Join(hostState, "trajectories", "trajectory-standalone_"+id+".ndjson"),
			start+"\n"+`{"type":"agent_message_chunk","agent_message_chunk":{"text":"hello"}}`+"\n")
	}

	// Project A: two sessions with trajectories.
	writePoolSession(sessA1)
	writePoolTrajectory(sessA1, projA)
	writePoolSession(sessA2)
	writePoolTrajectory(sessA2, projA)

	// Project B: one session with trajectory.
	writePoolSession(sessB1)
	writePoolTrajectory(sessB1, projB)

	// Per-project prompt history.
	writeFile(t, filepath.Join(hostState, "pool", claudeProjectSlug(projA), "prompt-history.json"),
		`[{"text":"prompt a"}]`)
	writeFile(t, filepath.Join(hostState, "pool", claudeProjectSlug(projB), "prompt-history.json"),
		`[{"text":"prompt b"}]`)

	// Per-project logs.
	writeFile(t, filepath.Join(hostState, "pool", "logs", claudeProjectSlug(projA), sessA1, "acp.log.jsonl"),
		`{"log":"a1"}`)
	writeFile(t, filepath.Join(hostState, "pool", "logs", claudeProjectSlug(projB), sessB1, "acp.log.jsonl"),
		`{"log":"b1"}`)

	return hostHome, hostState
}

// TestPreparePoolStoreSeedsOnlyCurrentProject verifies that the shadow state
// for workspace A contains A's sessions, trajectories, prompt history, and
// logs — and nothing from workspace B.
func TestPreparePoolStoreSeedsOnlyCurrentProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	projA := "/home/joe/src/flar"
	projB := "/home/joe/src/other"
	hostHome, _ := poolTestHome(t, projA, projB)

	store, err := preparePoolStore(hostHome, projA)
	if err != nil {
		t.Fatalf("preparePoolStore: %v", err)
	}

	// Sessions for A are present, B's is not.
	mustExist(t, store, filepath.Join("sessions", "session-019f8828-6c7b-7800-8f77-9388a5986887.json"))
	mustExist(t, store, filepath.Join("sessions", "session-019f8829-6c7b-7800-8f77-9388a5986888.json"))
	mustAbsent(t, store, filepath.Join("sessions", "session-019f8830-6c7b-7800-8f77-9388a5986889.json"))

	// Trajectories for A are present, B's is not.
	mustExist(t, store, filepath.Join("trajectories", "trajectory-standalone_019f8828-6c7b-7800-8f77-9388a5986887.ndjson"))
	mustExist(t, store, filepath.Join("trajectories", "trajectory-standalone_019f8829-6c7b-7800-8f77-9388a5986888.ndjson"))
	mustAbsent(t, store, filepath.Join("trajectories", "trajectory-standalone_019f8830-6c7b-7800-8f77-9388a5986889.ndjson"))

	// Trajectory content is preserved.
	trajData, err := os.ReadFile(filepath.Join(store, "trajectories", "trajectory-standalone_019f8828-6c7b-7800-8f77-9388a5986887.ndjson"))
	if err != nil {
		t.Fatalf("read trajectory: %v", err)
	}
	if !strings.Contains(string(trajData), projA) {
		t.Errorf("trajectory content missing working_directories for %s", projA)
	}
	if strings.Contains(string(trajData), projB) {
		t.Errorf("trajectory content leaked project B path: %s", trajData)
	}

	// Per-project prompt history: A present, B absent.
	mustExist(t, store, filepath.Join("pool", claudeProjectSlug(projA), "prompt-history.json"))
	mustAbsent(t, store, filepath.Join("pool", claudeProjectSlug(projB), "prompt-history.json"))

	// Per-project logs: A present, B absent.
	mustExist(t, store, filepath.Join("pool", "logs", claudeProjectSlug(projA), "019f8828-6c7b-7800-8f77-9388a5986887", "acp.log.jsonl"))
	mustAbsent(t, store, filepath.Join("pool", "logs", claudeProjectSlug(projB)))

	// Seed marker.
	mustExist(t, store, ".seeded")
}

// TestPreparePoolStoreSeedsOnce verifies the fork semantics: after the
// one-time seed, later host-side sessions are not pulled in and store
// contents are not clobbered.
func TestPreparePoolStoreSeedsOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	projA := "/home/joe/src/flar"
	hostHome, hostState := poolTestHome(t, projA, "/home/joe/src/other")

	store, err := preparePoolStore(hostHome, projA)
	if err != nil {
		t.Fatalf("preparePoolStore: %v", err)
	}

	// Simulate a sandboxed edit to a session after seeding.
	edited := filepath.Join(store, "sessions", "session-019f8828-6c7b-7800-8f77-9388a5986887.json")
	if err := os.WriteFile(edited, []byte("edited-in-flar"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A new host-side session should NOT be pulled in after seeding.
	newID := "019f8899-6c7b-7800-8f77-9388a5986899"
	writeFile(t, filepath.Join(hostState, "sessions", "session-"+newID+".json"), `{"session_id":"`+newID+`"}`)
	writeFile(t, filepath.Join(hostState, "trajectories", "trajectory-standalone_"+newID+".ndjson"),
		`{"type":"session.start","session_start":{"working_directories":["`+projA+`"]}}`)

	again, err := preparePoolStore(hostHome, projA)
	if err != nil || again != store {
		t.Fatalf("second preparePoolStore = %q, %v; want %q", again, err, store)
	}

	got, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "edited-in-flar" {
		t.Errorf("second preparePoolStore re-seeded and clobbered session; got %q", got)
	}
	mustAbsent(t, store, filepath.Join("sessions", "session-"+newID+".json"))
	mustAbsent(t, store, filepath.Join("trajectories", "trajectory-standalone_"+newID+".ndjson"))
}

// TestPreparePoolStoreFreshHost verifies seeding against a host with no pool
// state at all: the store still gets the directory structure and marker.
func TestPreparePoolStoreFreshHost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	hostHome := t.TempDir()

	store, err := preparePoolStore(hostHome, "/home/joe/src/flar")
	if err != nil {
		t.Fatalf("preparePoolStore: %v", err)
	}
	mustExist(t, store, "sessions")
	mustExist(t, store, "trajectories")
	mustExist(t, store, "pool")
	mustExist(t, store, filepath.Join("pool", "logs"))
	mustExist(t, store, ".seeded")
}

// TestPoolTrajSessionID verifies extraction of the session UUID from
// trajectory filenames.
func TestPoolTrajSessionID(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		{"trajectory-standalone_019f8828-6c7b-7800-8f77-9388a5986887.ndjson", "019f8828-6c7b-7800-8f77-9388a5986887"},
		{"trajectory-standalone_019f8829-6c7b-7800-8f77-9388a5986888.ndjson", "019f8829-6c7b-7800-8f77-9388a5986888"},
		{"trajectory-other_019f8830-6c7b-7800-8f77-9388a5986889.ndjson", "019f8830-6c7b-7800-8f77-9388a5986889"},
		{"not-a-trajectory.txt", ""},
		{"trajectory-standalone_no-uuid.ndjson", ""},
	}
	for _, c := range cases {
		m := poolTrajSessionID.FindStringSubmatch(c.filename)
		var got string
		if m != nil {
			got = m[1]
		}
		if got != c.want {
			t.Errorf("poolTrajSessionID(%q) = %q, want %q", c.filename, got, c.want)
		}
	}
}

// TestPoolStateDir verifies the host state directory resolution.
func TestPoolStateDir(t *testing.T) {
	home := "/home/test"

	// Default (no XDG_STATE_HOME).
	t.Setenv("XDG_STATE_HOME", "")
	if got, want := poolStateDir(home), "/home/test/.local/state/poolside"; got != want {
		t.Errorf("poolStateDir (default) = %q, want %q", got, want)
	}

	// With XDG_STATE_HOME set.
	t.Setenv("XDG_STATE_HOME", "/var/lib/state")
	if got, want := poolStateDir(home), "/var/lib/state/poolside"; got != want {
		t.Errorf("poolStateDir (XDG) = %q, want %q", got, want)
	}

	// Relative XDG_STATE_HOME is invalid, falls back.
	t.Setenv("XDG_STATE_HOME", "relative")
	if got, want := poolStateDir(home), "/home/test/.local/state/poolside"; got != want {
		t.Errorf("poolStateDir (relative XDG) = %q, want %q", got, want)
	}
}

// TestPoolConfigDir verifies the host config directory resolution.
func TestPoolConfigDir(t *testing.T) {
	home := "/home/test"

	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := poolConfigDir(home), "/home/test/.config/poolside"; got != want {
		t.Errorf("poolConfigDir (default) = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "/etc/config")
	if got, want := poolConfigDir(home), "/etc/config/poolside"; got != want {
		t.Errorf("poolConfigDir (XDG) = %q, want %q", got, want)
	}
}

// TestPreparePoolStoreSkipsUnattributableSessions verifies that sessions
// whose trajectories lack working_directories (or don't match the current
// project) are not copied.
func TestPreparePoolStoreSkipsUnattributableSessions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	projA := "/home/joe/src/flar"
	hostHome := t.TempDir()
	hostState := filepath.Join(hostHome, ".local", "state", "poolside")

	// A session with no working_directories in its trajectory.
	sessNoWd := "019f88aa-6c7b-7800-8f77-9388a5986aaa"
	writeFile(t, filepath.Join(hostState, "sessions", "session-"+sessNoWd+".json"),
		`{"session_id":"`+sessNoWd+`"}`)
	writeFile(t, filepath.Join(hostState, "trajectories", "trajectory-standalone_"+sessNoWd+".ndjson"),
		`{"type":"session.start","session_start":{"working_directories":[]}}`)

	// A session with working_directories for a different project.
	sessOther := "019f88bb-6c7b-7800-8f77-9388a5986bbb"
	writeFile(t, filepath.Join(hostState, "sessions", "session-"+sessOther+".json"),
		`{"session_id":"`+sessOther+`"}`)
	writeFile(t, filepath.Join(hostState, "trajectories", "trajectory-standalone_"+sessOther+".ndjson"),
		`{"type":"session.start","session_start":{"working_directories":["/some/other/path"]}}`)

	// A session with a malformed first line.
	sessBad := "019f88cc-6c7b-7800-8f77-9388a5986ccc"
	writeFile(t, filepath.Join(hostState, "sessions", "session-"+sessBad+".json"),
		`{"session_id":"`+sessBad+`"}`)
	writeFile(t, filepath.Join(hostState, "trajectories", "trajectory-standalone_"+sessBad+".ndjson"),
		`not json at all`)

	store, err := preparePoolStore(hostHome, projA)
	if err != nil {
		t.Fatalf("preparePoolStore: %v", err)
	}

	// None of these sessions should be in the shadow store.
	mustAbsent(t, store, filepath.Join("sessions", "session-"+sessNoWd+".json"))
	mustAbsent(t, store, filepath.Join("sessions", "session-"+sessOther+".json"))
	mustAbsent(t, store, filepath.Join("sessions", "session-"+sessBad+".json"))
	mustAbsent(t, store, filepath.Join("trajectories", "trajectory-standalone_"+sessNoWd+".ndjson"))
	mustAbsent(t, store, filepath.Join("trajectories", "trajectory-standalone_"+sessOther+".ndjson"))
	mustAbsent(t, store, filepath.Join("trajectories", "trajectory-standalone_"+sessBad+".ndjson"))
}

// TestPreparePoolStoreMarkerErrorPropagated verifies that when the .seeded
// marker cannot be created (e.g. the store directory is not writable),
// preparePoolStore returns an error instead of silently succeeding. Without
// this, a failed marker creation would cause the next run to re-seed and
// clobber sessions the user continued inside flar.
func TestPreparePoolStoreMarkerErrorPropagated(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping when running as root: read-only permissions do not apply")
	}

	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	hostHome := t.TempDir()
	absProjectDir := "/home/joe/src/flar"
	slug := claudeProjectSlug(absProjectDir)
	store := filepath.Join(flarStateDir(hostHome), "pool", slug)

	// Pre-create the directory structure so MkdirAll inside preparePoolStore
	// succeeds (it returns nil for existing directories).
	for _, sub := range []string{"sessions", "trajectories", "pool", filepath.Join("pool", "logs")} {
		if err := os.MkdirAll(filepath.Join(store, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Make the store directory read-only so the .seeded marker cannot be
	// created. Restore permissions on cleanup so t.TempDir() can remove it.
	if err := os.Chmod(store, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(store, 0o700) })

	// The host has no pool state, so seedPoolStore is a no-op and succeeds.
	// The failure should come from marker creation.
	_, err := preparePoolStore(hostHome, absProjectDir)
	if err == nil {
		t.Fatal("expected error when .seeded marker creation fails, got nil")
	}
	if !strings.Contains(err.Error(), "seed marker") {
		t.Errorf("expected error to mention seed marker, got: %v", err)
	}
}
