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
			if err := CopyDir(srcCodex, destCodex); err != nil {
				os.RemoveAll(tempDir)
				return "", err
			}
		}

	case AgentAgy:
		srcAgy := filepath.Join(home, ".gemini")
		if _, err := os.Stat(srcAgy); err == nil {
			destAgy := filepath.Join(tempDir, ".gemini")
			if err := CopyDir(srcAgy, destAgy); err != nil {
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
			if err := CopyDir(srcCopilot, destCopilot); err != nil {
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
	}

	return tempDir, nil
}

// claudeConfigAllowlist enumerates the entries under ~/.claude that the sandboxed
// agent legitimately needs. Everything not listed — notably projects/ (transcripts
// of OTHER projects), history.jsonl, sessions/, shell-snapshots/, and file-history/
// — is cross-project data the agent has no reason to read, so it is left out. The
// current project's own transcripts are copied separately (see copyClaudeConfig).
var claudeConfigAllowlist = []string{
	".credentials.json",   // OAuth token; required for auth
	"settings.json",       // user settings
	"settings.local.json", // local user settings, if present
	"CLAUDE.md",           // global user instructions
	"plugins",             // installed plugins
	"skills",              // installed skills
	"commands",            // custom slash commands
}

// copyClaudeConfig copies only the allowlisted entries of ~/.claude into dst, plus
// the projects/ subdirectory for the current project alone (used for session
// resume). It deliberately does not mirror the whole directory, which would expose
// every other project's conversation history to the sandboxed agent.
func copyClaudeConfig(src, dst, absProjectDir string) error {
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
	// Scope projects/ to just the current project's transcripts so --resume works
	// without dragging in other projects' history.
	slug := claudeProjectSlug(absProjectDir)
	srcProj := filepath.Join(src, "projects", slug)
	if info, err := os.Stat(srcProj); err == nil && info.IsDir() {
		if err := CopyDir(srcProj, filepath.Join(dst, "projects", slug)); err != nil {
			return err
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
