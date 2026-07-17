package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// kimiSkipCopy lists paths under ~/.kimi-code (relative to it) that flar does
// NOT copy into the sandbox config:
//   - sessions, session_index.jsonl, user-history, workspaces.json: session
//     state that is global on disk and mixes every project. The current
//     project's share is supplied at run time by the scoped shadow home (see
//     prepareKimiStore); copying it here would leak other projects' history.
//   - credentials, oauth: live OAuth tokens. These are bind-mounted directly
//     from the host at run time so the sandbox never persists a stale snapshot.
//   - logs, telemetry: global files mixing every project's activity.
//   - bin: holds the ~150MB kimi executable itself, which the generic
//     agent-binary mount supplies read-only at run time.
//   - updates: self-update state, which must stay per-host.
var kimiSkipCopy = map[string]bool{
	"sessions":            true,
	"session_index.jsonl": true,
	"user-history":        true,
	"workspaces.json":     true,
	"credentials":         true,
	"oauth":               true,
	"logs":                true,
	"telemetry":           true,
	"bin":                 true,
	"updates":             true,
}

// kimiPromptMode reports whether args request a non-interactive prompt run
// (-p/--prompt). kimi rejects --yolo and --auto in that mode, so flar omits
// the bypass flag when it is detected.
func kimiPromptMode(args []string) bool {
	for _, a := range args {
		if a == "-p" || a == "--prompt" || strings.HasPrefix(a, "--prompt=") {
			return true
		}
	}
	return false
}

// kimiSessionIndexEntry is one line of ~/.kimi-code/session_index.jsonl. Kimi
// resolves --continue/--session through this global index; a session whose
// line is missing cannot be loaded at all. sessionDir is an absolute path,
// which is identical inside the sandbox because HOME and the mount point match
// the host.
type kimiSessionIndexEntry struct {
	SessionID  string `json:"sessionId"`
	SessionDir string `json:"sessionDir"`
	WorkDir    string `json:"workDir"`
}

// prepareKimiStore returns a persistent, project-only Kimi Code home. It is
// seeded once from the trusted host store and never merged back: after Kimi
// runs in the sandbox every file in this directory must be treated as
// attacker-controlled. The store is bound into the sandbox as ~/.kimi-code, so
// sessions created in flar persist and can be resumed with `kimi --continue` /
// `--session`, while other projects' sessions stay invisible.
func prepareKimiStore(hostHome, absProjectDir, configSrc string) (string, error) {
	hostKimi := filepath.Join(hostHome, ".kimi-code")
	store := filepath.Join(flarStateDir(hostHome), "kimi", claudeProjectSlug(absProjectDir))
	marker := filepath.Join(store, ".seeded")
	if fileExists(marker) {
		return store, nil
	}
	if err := CopyDirExcept(configSrc, store, kimiSkipCopy); err != nil {
		return "", err
	}
	for _, sub := range []string{"credentials", "oauth"} {
		if err := os.MkdirAll(filepath.Join(store, sub), 0o700); err != nil {
			return "", err
		}
	}
	if err := seedKimiStore(hostKimi, store, absProjectDir); err != nil {
		return "", err
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return "", err
	}
	return store, nil
}

// seedKimiStore copies this project's existing host sessions into its scoped
// store, along with a scoped session index, this project's prompt history, and
// a workspaces.json filtered down to this project.
func seedKimiStore(hostKimi, store, absProjectDir string) error {
	hostSessions := filepath.Join(hostKimi, "sessions")
	index := kimiHostIndex(hostKimi, absProjectDir)

	// rels maps each session id attributed to this project to its directory
	// relative to sessions/ (<workspace-id>/<session-id>).
	rels := map[string]string{}

	// Primary attribution: each session's own state.json records its workDir.
	if wsEntries, err := os.ReadDir(hostSessions); err == nil {
		for _, ws := range wsEntries {
			if !ws.IsDir() {
				continue
			}
			wsDir := filepath.Join(hostSessions, ws.Name())
			sessEntries, err := os.ReadDir(wsDir)
			if err != nil {
				continue
			}
			for _, sess := range sessEntries {
				if !sess.IsDir() {
					continue
				}
				data, err := os.ReadFile(filepath.Join(wsDir, sess.Name(), "state.json"))
				if err != nil {
					continue
				}
				var state struct {
					WorkDir string `json:"workDir"`
				}
				if json.Unmarshal(data, &state) != nil || state.WorkDir != absProjectDir {
					continue
				}
				rels[sess.Name()] = filepath.Join(ws.Name(), sess.Name())
			}
		}
	}

	// Secondary attribution: index lines naming this workDir whose session dir
	// exists but was missed above (e.g. state.json lost). The index is trusted
	// host-side data, but the sessionDir it carries is still confined to
	// sessions/ before use.
	for id, raw := range index {
		if _, ok := rels[id]; ok {
			continue
		}
		var e kimiSessionIndexEntry
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		rel, err := filepath.Rel(hostSessions, e.SessionDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if !dirExists(e.SessionDir) {
			continue
		}
		rels[id] = rel
	}

	// Copy the matched sessions and rebuild a scoped index. Host index lines
	// are reused verbatim where present; the absolute sessionDir they carry
	// resolves identically inside the sandbox, so no rewriting is needed.
	ids := make([]string, 0, len(rels))
	for id := range rels {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var scoped bytes.Buffer
	for _, id := range ids {
		if err := CopyDir(filepath.Join(hostSessions, rels[id]), filepath.Join(store, "sessions", rels[id])); err != nil {
			return err
		}
		if raw, ok := index[id]; ok {
			scoped.Write(bytes.TrimSpace(raw))
			scoped.WriteByte('\n')
		} else if out, err := json.Marshal(kimiSessionIndexEntry{
			SessionID:  id,
			SessionDir: filepath.Join(hostKimi, "sessions", rels[id]),
			WorkDir:    absProjectDir,
		}); err == nil {
			scoped.Write(out)
			scoped.WriteByte('\n')
		}
	}

	if err := os.MkdirAll(filepath.Join(store, "sessions"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(store, "session_index.jsonl"), scoped.Bytes(), 0o600); err != nil {
		return err
	}

	// Prompt history is already project-scoped on disk: one file per workDir,
	// named md5(workDir).
	if err := os.MkdirAll(filepath.Join(store, "user-history"), 0o700); err != nil {
		return err
	}
	historyName := fmt.Sprintf("%x.jsonl", md5.Sum([]byte(absProjectDir)))
	if src := filepath.Join(hostKimi, "user-history", historyName); fileExists(src) {
		if err := CopyFile(src, filepath.Join(store, "user-history", historyName)); err != nil {
			return err
		}
	}

	return seedKimiWorkspaces(hostKimi, store, absProjectDir)
}

// kimiHostIndex returns the raw session_index.jsonl lines whose workDir is
// absProjectDir, keyed by session id. The index is the global resume table for
// every project; only this project's lines are carried into the scoped store.
// Missing or malformed input yields an empty map (best effort).
func kimiHostIndex(hostKimi, absProjectDir string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	f, err := os.Open(filepath.Join(hostKimi, "session_index.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var e kimiSessionIndexEntry
		if json.Unmarshal(line, &e) != nil || e.WorkDir != absProjectDir || e.SessionID == "" {
			continue
		}
		out[e.SessionID] = append([]byte{}, line...)
	}
	return out
}

// seedKimiWorkspaces writes a scoped workspaces.json holding only the entries
// whose root is the current project. The host file maps every workspace Kimi
// has ever opened to its root path; other projects' paths stay out of the
// sandbox. Absent or unparseable host data simply yields no file; Kimi
// recreates it on demand.
func seedKimiWorkspaces(hostKimi, store, absProjectDir string) error {
	data, err := os.ReadFile(filepath.Join(hostKimi, "workspaces.json"))
	if err != nil {
		return nil
	}
	var top struct {
		Version    int                        `json:"version"`
		Workspaces map[string]json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(data, &top) != nil {
		return nil
	}
	filtered := map[string]json.RawMessage{}
	for id, raw := range top.Workspaces {
		var ws struct {
			Root string `json:"root"`
		}
		if json.Unmarshal(raw, &ws) == nil && ws.Root == absProjectDir {
			filtered[id] = raw
		}
	}
	out, err := json.Marshal(struct {
		Version             int                        `json:"version"`
		Workspaces          map[string]json.RawMessage `json:"workspaces"`
		DeletedWorkspaceIDs []string                   `json:"deleted_workspace_ids"`
	}{top.Version, filtered, []string{}})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(store, "workspaces.json"), out, 0o600)
}
