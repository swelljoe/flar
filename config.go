package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PrepareConfigDir prepares a temporary directory with the agent's and git's configurations.
// absProjectDir is the workspace path; it scopes per-project agent state (transcripts,
// prompt history) to the current project so other projects' data never enters the sandbox.
// Returns the path of the temporary directory. The caller is responsible for deleting it.
func PrepareConfigDir(agent Agent, absProjectDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	// Create a temporary parent directory
	tempDir, err := os.MkdirTemp("", "flar-config-*")
	if err != nil {
		return "", err
	}

	// Create a .gitconfig file copy if it exists on the host
	hostGitConfig := filepath.Join(home, ".gitconfig")
	if _, err := os.Stat(hostGitConfig); err == nil {
		destGitConfig := filepath.Join(tempDir, ".gitconfig")
		if err := CopyFile(hostGitConfig, destGitConfig); err != nil {
			os.RemoveAll(tempDir)
			return "", err
		}
	}

	// Copy agent-specific files
	switch agent {
	case AgentClaude:
		srcClaude := filepath.Join(home, ".claude")
		if _, err := os.Stat(srcClaude); err == nil {
			destClaude := filepath.Join(tempDir, ".claude")
			if err := copyClaudeConfig(srcClaude, destClaude, absProjectDir); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}
		// ~/.claude.json holds onboarding state and the OAuth account; without it
		// Claude treats the sandbox as a fresh install and forces re-login. It also
		// carries a per-project map of prompt history and state for every project
		// ever opened, so copy it with that map filtered to the current project.
		srcClaudeJSON := filepath.Join(home, ".claude.json")
		if _, err := os.Stat(srcClaudeJSON); err == nil {
			destClaudeJSON := filepath.Join(tempDir, ".claude.json")
			if err := copyClaudeJSON(srcClaudeJSON, destClaudeJSON, absProjectDir); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

	case AgentCodex:
		srcCodex := filepath.Join(home, ".codex")
		if _, err := os.Stat(srcCodex); err == nil {
			destCodex := filepath.Join(tempDir, ".codex")
			// Conversation state and flar's shadow homes are supplied at run
			// time by prepareCodexStore. Copying either here would expose other
			// projects' sessions to the sandbox.
			if err := CopyDirExcept(srcCodex, destCodex, codexSkipCopy); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

	case AgentAgy:
		srcAgy := filepath.Join(home, ".gemini")
		if _, err := os.Stat(srcAgy); err == nil {
			destAgy := filepath.Join(tempDir, ".gemini")
			// Skip the conversation data and flar's per-workspace stores: the
			// current workspace's conversations are supplied at run time by a
			// scoped store bind (see prepareAgyStore), and copying the rest would
			// pull every other project's conversation history into the sandbox.
			if err := CopyDirExcept(srcAgy, destAgy, agySkipCopy); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}
		// agy keeps its OAuth token in the OS keyring (Secret Service), not a
		// file. Extract just that one secret so the sandbox can serve it via a
		// private Secret Service without exposing the whole keyring.
		if token, err := extractAgyToken(); err == nil && token != "" {
			dest := filepath.Join(tempDir, agySecretFile)
			_ = os.WriteFile(dest, []byte(token), 0o600)
		}

	case AgentCopilot:
		srcCopilot := filepath.Join(home, ".copilot")
		if _, err := os.Stat(srcCopilot); err == nil {
			destCopilot := filepath.Join(tempDir, ".copilot")
			// Copilot keeps resumable sessions in a global SQLite store plus
			// per-session directories. Those are supplied at run time by a
			// project-scoped shadow home, so copying them here would leak other
			// projects' sessions into the sandbox.
			if err := CopyDirExcept(srcCopilot, destCopilot, copilotSkipCopy); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}
		srcGh := filepath.Join(home, ".config", "gh")
		if _, err := os.Stat(srcGh); err == nil {
			destGh := filepath.Join(tempDir, "gh")
			if err := CopyDir(srcGh, destGh); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

	case AgentReasonix:
		srcReasonix := filepath.Join(home, ".reasonix")
		if _, err := os.Stat(srcReasonix); err == nil {
			destReasonix := filepath.Join(tempDir, ".reasonix")
			// Reasonix sessions are already project-scoped on disk
			// (~/.reasonix/projects/<slug>/), so flar copies only the global
			// config and secrets, then live-binds the current project's
			// directory at run time — the same pattern used for Claude.
			// projects/ and sessions/ are deliberately skipped: the current
			// project's sessions are live-bound from the host, and copying them
			// here would expose every other project's session history.
			if err := CopyDirExcept(srcReasonix, destReasonix, reasonixSkipCopy); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

	case AgentKimi:
		srcKimi := filepath.Join(home, ".kimi-code")
		if _, err := os.Stat(srcKimi); err == nil {
			destKimi := filepath.Join(tempDir, ".kimi-code")
			// Kimi's session state is global on disk and mixes every project,
			// so it is supplied at run time by a project-scoped shadow home
			// (see prepareKimiStore); copying it here would leak other
			// projects' history. Its live OAuth dirs are skipped too and
			// bound from the host at run time so flar never persists a stale
			// credential snapshot. bin/ (the kimi executable itself) and
			// updates/ are skipped as well: the real binary is bind-mounted
			// read-only at run time.
			if err := CopyDirExcept(srcKimi, destKimi, kimiSkipCopy); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

	case AgentPool:
		srcPool := poolConfigDir(home)
		if _, err := os.Stat(srcPool); err == nil {
			destPool := filepath.Join(tempDir, "poolside")
			// Pool keeps config (credentials, settings, skills) under
			// ~/.config/poolside and state (sessions, trajectories,
			// per-project prompt history/logs) under ~/.local/state/poolside.
			// Only the config is copied here; the state is forked per-project
			// at run time by preparePoolStore so other projects' sessions and
			// trajectories never enter the sandbox.
			if err := CopyDir(srcPool, destPool); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

	case AgentQwen:
		srcQwen := filepath.Join(home, ".qwen")
		if _, err := os.Stat(srcQwen); err == nil {
			destQwen := filepath.Join(tempDir, ".qwen")
			// Qwen already scopes sessions per-project on disk
			// (~/.qwen/projects/<slug>/), so flar copies only the global
			// config and secrets, then live-binds the current project's
			// directory at run time — the same pattern used for Claude and
			// Reasonix. projects/, tmp/, and usage/ are deliberately skipped:
			// the current project's sessions are live-bound from the host,
			// and copying them here would expose every other project's
			// session history.
			if err := CopyDirExcept(srcQwen, destQwen, qwenSkipCopy); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

	case AgentMimo:
		// mimo's user config lives in ~/.config/mimocode/ (or
		// $XDG_CONFIG_HOME/mimocode/). Copy it excluding installation
		// scaffolding (node_modules, package.json).
		srcMimoCfg := mimoConfigDir(home)
		if _, err := os.Stat(srcMimoCfg); err == nil {
			destMimoCfg := filepath.Join(tempDir, "mimocode-config")
			if err := CopyDirExcept(srcMimoCfg, destMimoCfg, mimoConfigSkipCopy); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

		// mimo's data directory (~/.local/share/mimocode/) contains the global
		// SQLite database, auth credentials, and other data. The database mixes
		// every project's sessions and is replaced at run time by a per-project
		// shadow store (see prepareMimoStore). Memory, logs, and snapshots are
		// also skipped: memory is live-bound per project, and logs/snapshots
		// mix all projects. Only auth.json, installation_id, mimo-key-name,
		// trusted-workspaces.json, builtin_skills/, compose/, and storage/ are
		// copied.
		srcMimoData := mimoDataDir(home)
		if _, err := os.Stat(srcMimoData); err == nil {
			destMimoData := filepath.Join(tempDir, "mimocode-data")
			if err := CopyDirExcept(srcMimoData, destMimoData, mimoSkipCopy); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}
	}

	return tempDir, nil
}

// claudeConfigAllowlist enumerates the entries under ~/.claude that the sandboxed
// agent legitimately needs. Everything not listed — notably projects/ (transcripts
// of OTHER projects), history.jsonl, sessions/, shell-snapshots/, and file-history/
// — is cross-project data the agent has no reason to read, so it is left out. The
// current project's own transcripts are not copied here; they are live-bound from
// the host (see RunSandbox) so sessions run in the sandbox persist and can resume.
var claudeConfigAllowlist = []string{
	".credentials.json",   // OAuth token; required for auth
	"settings.json",       // user settings
	"settings.local.json", // local user settings, if present
	"CLAUDE.md",           // global user instructions
	"plugins",             // installed plugins
	"skills",              // installed skills
	"commands",            // custom slash commands
}

// copyClaudeConfig copies only the allowlisted entries of ~/.claude into dst. It
// deliberately does not mirror the whole directory, which would expose every other
// project's conversation history to the sandboxed agent. The current project's own
// transcripts are not copied; RunSandbox live-binds them from the host so sessions
// run in the sandbox persist and can be resumed.
//
// absProjectDir is retained for signature symmetry with copyClaudeJSON and to keep
// the per-project scoping decision visible at the one call site.
func copyClaudeConfig(src, dst, absProjectDir string) error {
	_ = absProjectDir
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, name := range claudeConfigAllowlist {
		srcPath := filepath.Join(src, name)
		info, err := os.Stat(srcPath)
		if err != nil {
			continue // absent entries are fine
		}
		dstPath := filepath.Join(dst, name)
		if info.IsDir() {
			if err := CopyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else if info.Mode().IsRegular() {
			if err := CopyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// claudeProjectSlug reproduces Claude Code's cwd-to-directory encoding, which
// replaces every non-alphanumeric character with '-' (e.g. /home/joe/src/flar ->
// -home-joe-src-flar). Used to locate the current project's transcripts.
func claudeProjectSlug(absPath string) string {
	var b strings.Builder
	for _, r := range absPath {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// copyClaudeJSON copies ~/.claude.json but strips its per-project "projects" map
// down to the current project. That map holds prompt history and state for every
// project the user has ever opened; the sandbox needs only this one. All other
// top-level fields (OAuth account, onboarding state, caches) are preserved verbatim.
// If the file cannot be parsed, it falls back to a verbatim copy rather than risk
// breaking authentication.
func copyClaudeJSON(src, dst, absProjectDir string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return CopyFile(src, dst)
	}
	if raw, ok := top["projects"]; ok {
		var projects map[string]json.RawMessage
		if err := json.Unmarshal(raw, &projects); err == nil {
			filtered := map[string]json.RawMessage{}
			if entry, ok := projects[absProjectDir]; ok {
				filtered[absProjectDir] = entry
			}
			if newRaw, err := json.Marshal(filtered); err == nil {
				top["projects"] = newRaw
			}
		}
	}
	out, err := json.Marshal(top)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, out, 0o600)
}

// CopyFile copies a single file from src to dst.
func CopyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	return dstFile.Sync()
}

// agySkipCopy lists paths under ~/.gemini (relative to it) that flar does NOT copy
// into the sandbox config. The conversation directories and their indices are
// replaced at run time by this workspace's scoped store; agyStoreRel holds every
// workspace's scoped store and must never be exposed to another workspace.
var agySkipCopy = map[string]bool{
	filepath.Join("antigravity-cli", "conversations"):                    true,
	filepath.Join("antigravity-cli", "brain"):                            true,
	filepath.Join("antigravity-cli", "implicit"):                         true,
	filepath.Join("antigravity-cli", "history.jsonl"):                    true,
	filepath.Join("antigravity-cli", "cache", "last_conversations.json"): true,
	filepath.Join("antigravity-cli", agyStoreRel):                        true,
}

// copilotSkipCopy lists paths under ~/.copilot (relative to it) that flar does
// NOT copy into the sandbox config. Copilot's resumable session history lives in
// a global SQLite store plus session-state directories, so those are replaced at
// run time by a project-scoped shadow home; the shadow homes themselves also
// stay out of copied configs.
var copilotSkipCopy = map[string]bool{
	"session-state":        true,
	"session-store.db":     true,
	"session-store.db-wal": true,
	"session-store.db-shm": true,
	copilotStoreRel:        true,
}

// reasonixSkipCopy lists paths under ~/.reasonix (relative to it) that flar does
// NOT copy into the sandbox config. Reasonix already scopes sessions per-project
// on disk (~/.reasonix/projects/<slug>/), so the current project's directory is
// live-bound at run time. Copying projects/ or sessions/ here would leak other
// projects' history into the sandbox.
var reasonixSkipCopy = map[string]bool{
	"projects": true,
	"sessions": true,
}

// qwenSkipCopy lists paths under ~/.qwen (relative to it) that flar does NOT
// copy into the sandbox config. Qwen already scopes sessions per-project on
// disk (~/.qwen/projects/<slug>/), so the current project's directory is
// live-bound at run time — the same pattern used for Claude and Reasonix.
// Copying projects/ here would leak other projects' history into the sandbox.
// tmp/ holds per-project-hash runtime state and usage/ tracks global token
// consumption; neither is needed inside the sandbox.
var qwenSkipCopy = map[string]bool{
	"projects": true,
	"tmp":      true,
	"usage":    true,
}

// CopyDirExcept recursively copies src to dst, skipping any entry whose path
// relative to src is present in skip.
func CopyDirExcept(src, dst string, skip map[string]bool) error {
	return copyDirRel(src, dst, "", skip)
}

func copyDirRel(src, dst, rel string, skip map[string]bool) error {
	srcDir := filepath.Join(src, rel)
	info, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dst, rel), info.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRel := filepath.Join(rel, entry.Name())
		if skip[childRel] {
			continue
		}
		srcPath := filepath.Join(src, childRel)
		if entry.IsDir() {
			if err := copyDirRel(src, dst, childRel, skip); err != nil {
				return err
			}
		} else {
			fi, err := entry.Info()
			if err != nil {
				return err
			}
			if fi.Mode().IsRegular() {
				if err := CopyFile(srcPath, filepath.Join(dst, childRel)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// CopyDir recursively copies a directory from src to dst.
func CopyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := CopyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			// Skip symlinks or non-regular files
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				if err := CopyFile(srcPath, dstPath); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
