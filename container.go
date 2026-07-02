package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Agent represents the supported AI developer agents.
type Agent string

const (
	AgentClaude  Agent = "claude"
	AgentCodex   Agent = "codex"
	AgentAgy     Agent = "agy"
	AgentCopilot Agent = "copilot"
)

// RunOpts holds parameters for running the Bubblewrap sandbox.
type RunOpts struct {
	ProjectDir string
	TempConfig string
	TempNetDir string
	AllowPorts []int
	Agent      Agent
	Network    string // "isolated" or "host"
	AskMode    bool
	Verbose    bool
	ExtraArgs  []string
}

// RunSandbox runs the Bubblewrap sandbox with the specified options.
func RunSandbox(opts RunOpts) error {
	hostHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home directory: %w", err)
	}

	absProjectDir, err := filepath.Abs(opts.ProjectDir)
	if err != nil {
		return fmt.Errorf("failed to resolve absolute project path: %w", err)
	}

	// Determine agent command to run
	var agentCmd string
	switch opts.Agent {
	case AgentClaude:
		agentCmd = "claude"
	case AgentCodex:
		agentCmd = "codex"
	case AgentAgy:
		agentCmd = "agy"
	case AgentCopilot:
		agentCmd = "github-copilot-cli"
	default:
		return fmt.Errorf("unknown or unsupported agent: %s", opts.Agent)
	}

	// Resolve the agent executable path on the host
	hostAgentPath, err := exec.LookPath(agentCmd)
	if err != nil {
		// Fallback for agy if not in PATH
		if opts.Agent == AgentAgy {
			defaultPath := filepath.Join(hostHome, ".local", "bin", "agy")
			if _, err := os.Stat(defaultPath); err == nil {
				hostAgentPath = defaultPath
			}
		}
		if hostAgentPath == "" {
			return fmt.Errorf("agent binary %q not found on host; please ensure it is in your PATH", agentCmd)
		}
	}

	realAgentPath, err := filepath.EvalSymlinks(hostAgentPath)
	if err != nil {
		realAgentPath = hostAgentPath
	}

	// Prepare bubblewrap arguments
	bwrapArgs := []string{
		"--unshare-all",
	}

	// Share network if requested
	if opts.Network == "host" {
		bwrapArgs = append(bwrapArgs, "--share-net")
	}

	// Mount empty tmpfs on root
	bwrapArgs = append(bwrapArgs, "--tmpfs", "/")

	// Mount system directories read-only
	bwrapArgs = append(bwrapArgs,
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
	)

	// Bind-mount optional system paths if they exist
	optPaths := []string{"/opt", "/var", "/etc/resolv.conf", "/etc/hosts", "/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/etc/alternatives", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf"}
	for _, p := range optPaths {
		if _, err := os.Stat(p); err == nil {
			bwrapArgs = append(bwrapArgs, "--ro-bind-try", p, p)
		}
	}

	// Mount essential kernel filesystems
	bwrapArgs = append(bwrapArgs,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
	)

	// Bind-mount project directory (read-write)
	bwrapArgs = append(bwrapArgs, "--bind", absProjectDir, absProjectDir)

	// Setup HOME directory structure inside sandbox
	bwrapArgs = append(bwrapArgs, "--dir", hostHome)

	// Bind-mount agent configurations into the home directory if prepared
	if opts.TempConfig != "" {
		switch opts.Agent {
		case AgentClaude:
			claudePath := filepath.Join(opts.TempConfig, ".claude")
			if _, err := os.Stat(claudePath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", claudePath, filepath.Join(hostHome, ".claude"))
			}
		case AgentCodex:
			codexPath := filepath.Join(opts.TempConfig, ".codex")
			if _, err := os.Stat(codexPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", codexPath, filepath.Join(hostHome, ".codex"))
			}
		case AgentAgy:
			agyPath := filepath.Join(opts.TempConfig, ".gemini")
			if _, err := os.Stat(agyPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", agyPath, filepath.Join(hostHome, ".gemini"))
			}
		case AgentCopilot:
			copilotPath := filepath.Join(opts.TempConfig, ".copilot")
			if _, err := os.Stat(copilotPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", copilotPath, filepath.Join(hostHome, ".copilot"))
			}
			ghPath := filepath.Join(opts.TempConfig, "gh")
			if _, err := os.Stat(ghPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--dir", filepath.Join(hostHome, ".config"))
				bwrapArgs = append(bwrapArgs, "--bind", ghPath, filepath.Join(hostHome, ".config", "gh"))
			}
		}

		// Git config
		gitConfigPath := filepath.Join(opts.TempConfig, ".gitconfig")
		if _, err := os.Stat(gitConfigPath); err == nil {
			bwrapArgs = append(bwrapArgs, "--bind", gitConfigPath, filepath.Join(hostHome, ".gitconfig"))
		}
	}

	// Mount the host flar binary inside the sandbox
	hostFlar, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get flar executable path: %w", err)
	}
	realHostFlar, err := filepath.EvalSymlinks(hostFlar)
	if err != nil {
		realHostFlar = hostFlar
	}
	bwrapArgs = append(bwrapArgs, "--dir", "/usr/local/bin")
	bwrapArgs = append(bwrapArgs, "--ro-bind", realHostFlar, "/usr/local/bin/flar")

	// Mount agent binary if it's in the home directory or not under /usr /bin /sbin
	if !strings.HasPrefix(realAgentPath, "/usr/") && !strings.HasPrefix(realAgentPath, "/bin/") && !strings.HasPrefix(realAgentPath, "/sbin/") {
		agentDir := filepath.Dir(hostAgentPath)
		var dirs []string
		curr := "/"
		for _, part := range strings.Split(agentDir, "/") {
			if part == "" {
				continue
			}
			curr = filepath.Join(curr, part)
			dirs = append(dirs, curr)
		}
		for _, d := range dirs {
			bwrapArgs = append(bwrapArgs, "--dir", d)
		}
		bwrapArgs = append(bwrapArgs, "--ro-bind", realAgentPath, hostAgentPath)
	}

	// Mount local network proxy directory if isolated network
	if opts.Network == "isolated" && opts.TempNetDir != "" {
		bwrapArgs = append(bwrapArgs, "--bind", opts.TempNetDir, "/run/flar-net")
	}

	// Pass environment variables
	bwrapArgs = append(bwrapArgs, "--setenv", "HOME", hostHome)
	envVars := []string{
		"PATH",
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
			bwrapArgs = append(bwrapArgs, "--setenv", env, val)
		}
	}

	// Setup proxies if isolated network
	if opts.Network == "isolated" {
		bwrapArgs = append(bwrapArgs,
			"--setenv", "HTTP_PROXY", "http://127.0.0.1:9090",
			"--setenv", "HTTPS_PROXY", "http://127.0.0.1:9090",
			"--setenv", "http_proxy", "http://127.0.0.1:9090",
			"--setenv", "https_proxy", "http://127.0.0.1:9090",
		)
	}

	// Construct the agent command line and bypass flags
	var agentArgs []string
	switch opts.Agent {
	case AgentClaude:
		agentArgs = append(agentArgs, "claude")
		if !opts.AskMode {
			agentArgs = append(agentArgs, "--dangerously-skip-permissions")
		}
	case AgentCodex:
		agentArgs = append(agentArgs, "codex")
		if !opts.AskMode {
			agentArgs = append(agentArgs, "--dangerously-bypass-approvals-and-sandbox")
		}
	case AgentAgy:
		agentArgs = append(agentArgs, "agy")
		if !opts.AskMode {
			agentArgs = append(agentArgs, "--dangerously-skip-permissions")
		}
	case AgentCopilot:
		agentArgs = append(agentArgs, "github-copilot-cli")
	}

	if len(opts.ExtraArgs) > 0 {
		agentArgs = append(agentArgs, opts.ExtraArgs...)
	}

	// Prepare script inside sandbox
	var bashScript strings.Builder
	if opts.Network == "isolated" {
		// Run HTTP/HTTPS proxy inside sandbox
		bashScript.WriteString("flar --internal-proxy 9090 /run/flar-net/http-proxy.sock &\n")
		// Run custom TCP proxies
		for _, port := range opts.AllowPorts {
			bashScript.WriteString(fmt.Sprintf("flar --internal-proxy %d /run/flar-net/port-%d.sock &\n", port, port))
		}
	}

	bashScript.WriteString("exec \"$@\"\n")

	// Append bash execution arguments to bwrap
	bwrapArgs = append(bwrapArgs,
		"--chdir", absProjectDir,
		"/bin/bash", "-c", bashScript.String(),
		"flar", // dummy $0
	)

	// Append agent command and args
	bwrapArgs = append(bwrapArgs, agentArgs...)

	if opts.Verbose {
		fmt.Printf("Running command: bwrap %s\n", strings.Join(bwrapArgs, " "))
	}

	cmd := exec.Command("bwrap", bwrapArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}
