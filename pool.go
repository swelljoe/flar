package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// poolConfigDir returns the host-side pool config directory (~/.config/poolside),
// respecting XDG_CONFIG_HOME.
func poolConfigDir(home string) string {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(configHome) {
		return filepath.Join(configHome, "poolside")
	}
	return filepath.Join(home, ".config", "poolside")
}

// poolStateDir returns the host-side pool state directory
// (~/.local/state/poolside), respecting XDG_STATE_HOME.
func poolStateDir(home string) string {
	if stateHome := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(stateHome) {
		return filepath.Join(stateHome, "poolside")
	}
	return filepath.Join(home, ".local", "state", "poolside")
}

// poolTrajSessionID extracts the session UUID from a pool trajectory filename.
// Trajectory filenames look like: trajectory-standalone_<uuid>.ndjson
// The session ID is a standard UUID (hex digits and hyphens), so splitting on
// the last underscore before the extension is safe.
var poolTrajSessionID = regexp.MustCompile(`_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.ndjson$`)

// poolSessionStartEvent is the first line of a pool trajectory file. It carries
// the working_directories array that attributes the session to a project.
type poolSessionStartEvent struct {
	Type         string `json:"type"`
	SessionStart struct {
		WorkingDirectories []string `json:"working_directories"`
	} `json:"session_start"`
}

// preparePoolStore returns a persistent, project-only pool state directory.
// It is seeded once from the trusted host store and never merged back: after
// pool runs in the sandbox, every file in this directory must be treated as
// attacker-controlled.
//
// Pool stores sessions and trajectories in two global directories that mix
// every project, so flar forks them into a per-project shadow home under
// $XDG_STATE_HOME/flar/pool/<slug>/. The shadow home is bind-mounted as
// ~/.local/state/poolside inside the sandbox, so sessions created in flar
// persist and can be resumed with `pool -r` / `--resume`, while other
// projects' sessions stay invisible.
func preparePoolStore(hostHome, absProjectDir string) (string, error) {
	hostState := poolStateDir(hostHome)
	store := filepath.Join(flarStateDir(hostHome), "pool", claudeProjectSlug(absProjectDir))

	// Create the directory structure the sandbox expects.
	for _, sub := range []string{"sessions", "trajectories", "pool", filepath.Join("pool", "logs")} {
		if err := os.MkdirAll(filepath.Join(store, sub), 0o700); err != nil {
			return "", err
		}
	}

	// Seed exactly once; the marker guards against re-seeding (which would
	// clobber sessions the user has since continued inside flar).
	marker := filepath.Join(store, ".seeded")
	if _, err := os.Stat(marker); err != nil {
		if err := seedPoolStore(hostState, store, absProjectDir); err != nil {
			return "", err
		}
		if f, err := os.OpenFile(marker, os.O_CREATE, 0o600); err == nil {
			f.Close()
		}
	}

	return store, nil
}

// seedPoolStore copies this project's existing host sessions and trajectories
// into the scoped store, along with this project's prompt history and logs.
// Only sessions attributed to absProjectDir (via working_directories in the
// trajectory's session.start event) are copied; all other projects' data is
// left behind.
func seedPoolStore(hostState, store, absProjectDir string) error {
	slug := claudeProjectSlug(absProjectDir)

	// Seed sessions and trajectories for the current project only.
	if err := seedPoolSessions(
		filepath.Join(hostState, "trajectories"),
		filepath.Join(hostState, "sessions"),
		filepath.Join(store, "trajectories"),
		filepath.Join(store, "sessions"),
		absProjectDir,
	); err != nil {
		return err
	}

	// Seed per-project prompt history (pool/<slug>/prompt-history.json).
	srcPromptHistory := filepath.Join(hostState, "pool", slug, "prompt-history.json")
	if fileExists(srcPromptHistory) {
		dstPromptHistory := filepath.Join(store, "pool", slug, "prompt-history.json")
		if err := os.MkdirAll(filepath.Dir(dstPromptHistory), 0o700); err != nil {
			return err
		}
		if err := CopyFile(srcPromptHistory, dstPromptHistory); err != nil {
			return err
		}
	}

	// Seed per-project logs (pool/logs/<slug>/).
	srcLogs := filepath.Join(hostState, "pool", "logs", slug)
	if dirExists(srcLogs) {
		dstLogs := filepath.Join(store, "pool", "logs", slug)
		if err := CopyDir(srcLogs, dstLogs); err != nil {
			return err
		}
	}

	return nil
}

// seedPoolSessions copies session files and their trajectories for sessions
// attributed to absProjectDir. Attribution is determined by reading each
// trajectory's session.start event and checking whether working_directories
// contains absProjectDir — never by parsing the conversation contents.
// Sessions without a trajectory (or whose trajectory can't be attributed) are
// omitted, which is the safe default for a security boundary.
func seedPoolSessions(hostTrajDir, hostSessionsDir, dstTrajDir, dstSessionsDir, absProjectDir string) error {
	entries, err := os.ReadDir(hostTrajDir)
	if err != nil {
		return nil // no trajectories on the host, nothing to seed
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ndjson") {
			continue
		}

		trajPath := filepath.Join(hostTrajDir, entry.Name())

		// Read only the first line: the session.start event.
		f, err := os.Open(trajPath)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		if !scanner.Scan() {
			f.Close()
			continue
		}
		var event poolSessionStartEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Type != "session.start" {
			f.Close()
			continue
		}
		f.Close()

		// Check if this session belongs to the current project.
		match := false
		for _, wd := range event.SessionStart.WorkingDirectories {
			if wd == absProjectDir {
				match = true
				break
			}
		}
		if !match {
			continue
		}

		// Copy the trajectory file.
		if err := CopyFile(trajPath, filepath.Join(dstTrajDir, entry.Name())); err != nil {
			return err
		}

		// Extract the session ID from the trajectory filename and copy the
		// corresponding session metadata file.
		if m := poolTrajSessionID.FindStringSubmatch(entry.Name()); m != nil {
			sessionFile := "session-" + m[1] + ".json"
			srcSession := filepath.Join(hostSessionsDir, sessionFile)
			if fileExists(srcSession) {
				if err := CopyFile(srcSession, filepath.Join(dstSessionsDir, sessionFile)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
