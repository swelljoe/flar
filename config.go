package main

import (
	"io"
	"os"
	"path/filepath"
)

// PrepareConfigDir prepares a temporary directory with the agent's and git's configurations.
// Returns the path of the temporary directory. The caller is responsible for deleting it.
func PrepareConfigDir(agent Agent) (string, error) {
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
			if err := CopyDir(srcClaude, destClaude); err != nil {
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
