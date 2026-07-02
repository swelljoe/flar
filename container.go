package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunOpts holds parameters for running the container.
type RunOpts struct {
	ImageName  string
	ProjectDir string
	TempConfig string
	Agent      Agent
	AskMode    bool
	Verbose    bool
	ExtraArgs  []string
}

// ImageExists checks if the specified Podman image exists.
func ImageExists(imageName string) (bool, error) {
	cmd := exec.Command("podman", "image", "exists", imageName)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	// Exit code of image exists is non-zero if it doesn't exist
	if _, ok := err.(*exec.ExitError); ok {
		return false, nil
	}
	return false, err
}

// BuildImage builds a Podman image using either a Containerfile path or inline content.
func BuildImage(imageName string, customPath string, inlineContent string, verbose bool) error {
	tempCtx, err := os.MkdirTemp("", "flar-build-*")
	if err != nil {
		return fmt.Errorf("failed to create temp context dir: %w", err)
	}
	defer os.RemoveAll(tempCtx)

	var cmd *exec.Cmd
	if customPath != "" {
		if verbose {
			fmt.Printf("Building image %s from custom Containerfile %s...\n", imageName, customPath)
		}
		cmd = exec.Command("podman", "build", "-t", imageName, "-f", customPath, tempCtx)
	} else {
		if verbose {
			fmt.Printf("Building image %s from built-in template...\n", imageName)
		}
		cmd = exec.Command("podman", "build", "-t", imageName, "-f", "-", tempCtx)
		cmd.Stdin = strings.NewReader(inlineContent)
	}

	if verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		defer func() {
			if cmd.ProcessState != nil && !cmd.ProcessState.Success() {
				fmt.Fprintf(os.Stderr, "Build logs:\n%s\n", errBuf.String())
			}
		}()
	}

	return cmd.Run()
}

// RunContainer runs the Podman container with the specified options.
func RunContainer(opts RunOpts) error {
	args := []string{"run", "--rm", "-it"}

	// Mount project directory
	absProjectDir, err := filepath.Abs(opts.ProjectDir)
	if err != nil {
		return fmt.Errorf("failed to resolve absolute project path: %w", err)
	}
	args = append(args, "-v", fmt.Sprintf("%s:/workspace:Z", absProjectDir))
	args = append(args, "-w", "/workspace")

	// Mount configurations
	if opts.TempConfig != "" {
		// Git config
		gitConfigPath := filepath.Join(opts.TempConfig, ".gitconfig")
		if _, err := os.Stat(gitConfigPath); err == nil {
			args = append(args, "-v", fmt.Sprintf("%s:/root/.gitconfig:Z", gitConfigPath))
		}

		// Agent configs
		switch opts.Agent {
		case AgentClaude:
			claudePath := filepath.Join(opts.TempConfig, ".claude")
			if _, err := os.Stat(claudePath); err == nil {
				args = append(args, "-v", fmt.Sprintf("%s:/root/.claude:Z", claudePath))
			}
		case AgentCodex:
			codexPath := filepath.Join(opts.TempConfig, ".codex")
			if _, err := os.Stat(codexPath); err == nil {
				args = append(args, "-v", fmt.Sprintf("%s:/root/.codex:Z", codexPath))
			}
		case AgentAgy:
			agyPath := filepath.Join(opts.TempConfig, ".gemini")
			if _, err := os.Stat(agyPath); err == nil {
				args = append(args, "-v", fmt.Sprintf("%s:/root/.gemini:Z", agyPath))
			}
		case AgentCopilot:
			copilotPath := filepath.Join(opts.TempConfig, ".copilot")
			if _, err := os.Stat(copilotPath); err == nil {
				args = append(args, "-v", fmt.Sprintf("%s:/root/.copilot:Z", copilotPath))
			}
			ghPath := filepath.Join(opts.TempConfig, "gh")
			if _, err := os.Stat(ghPath); err == nil {
				args = append(args, "-v", fmt.Sprintf("%s:/root/.config/gh:Z", ghPath))
			}
		}
	}

	// Mount host agy binary if running agy agent
	if opts.Agent == AgentAgy {
		hostAgy, err := exec.LookPath("agy")
		if err != nil {
			// Fallback to check default path
			home, _ := os.UserHomeDir()
			defaultPath := filepath.Join(home, ".local", "bin", "agy")
			if _, err := os.Stat(defaultPath); err == nil {
				hostAgy = defaultPath
			}
		}
		if hostAgy != "" {
			args = append(args, "-v", fmt.Sprintf("%s:/usr/local/bin/agy:ro", hostAgy))
		} else {
			return fmt.Errorf("agy binary not found on host; please install it or ensure it is in PATH")
		}
	}

	// Pass relevant environment variables
	envVars := []string{
		"TERM",
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"GEMINI_API_KEY",
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"COPILOT_GITHUB_TOKEN",
	}
	for _, env := range envVars {
		if val, exists := os.LookupEnv(env); exists {
			args = append(args, "-e", fmt.Sprintf("%s=%s", env, val))
		}
	}

	// Add image name
	args = append(args, opts.ImageName)

	// Determine container command and agent-specific bypass flags
	var agentCmd string
	switch opts.Agent {
	case AgentClaude:
		agentCmd = "claude"
		if !opts.AskMode {
			args = append(args, agentCmd, "--dangerously-skip-permissions")
		} else {
			args = append(args, agentCmd)
		}
	case AgentCodex:
		agentCmd = "codex"
		if !opts.AskMode {
			args = append(args, agentCmd, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			args = append(args, agentCmd)
		}
	case AgentAgy:
		agentCmd = "agy"
		if !opts.AskMode {
			args = append(args, agentCmd, "--dangerously-skip-permissions")
		} else {
			args = append(args, agentCmd)
		}
	case AgentCopilot:
		agentCmd = "github-copilot-cli"
		args = append(args, agentCmd)
	}

	// Append any extra arguments/prompts passed from the command line
	if len(opts.ExtraArgs) > 0 {
		args = append(args, opts.ExtraArgs...)
	}

	if opts.Verbose {
		fmt.Printf("Running command: podman %s\n", strings.Join(args, " "))
	}

	cmd := exec.Command("podman", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}
