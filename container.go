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
	AgentClaude   Agent = "claude"
	AgentCodex    Agent = "codex"
	AgentAgy      Agent = "agy"
	AgentCopilot  Agent = "copilot"
	AgentReasonix Agent = "reasonix"
	AgentKimi     Agent = "kimi"
	AgentPool     Agent = "pool"
	AgentQwen     Agent = "qwen"
)

// commonEnvVars is the set of non-secret host environment variables that flar
// forwards into every sandbox via bwrap --setenv.
//
// XDG_CONFIG_HOME and XDG_STATE_HOME are included so that pool (which
// resolves its config and state directories via these variables) looks in the
// same paths inside the sandbox where flar bind-mounted them. Without them,
// pool would fall back to the default ~/.config/poolside and
// ~/.local/state/poolside and miss the bind mounts when the user has a
// non-default XDG location.
var commonEnvVars = []string{
	"PATH",
	"TERM",
	"USER",
	"USERNAME",
	"LOGNAME",
	"XDG_CONFIG_HOME",
	"XDG_STATE_HOME",
}

// agentEnvVars maps each agent to the credential environment variables it
// needs to authenticate inside the sandbox. Only the variables for the agent
// actually being run are forwarded: flar exists to minimize the blast area of
// a prompt injection or supply-chain attack, and an agent has no legitimate
// reason to read another agent's API keys.
var agentEnvVars = map[Agent][]string{
	AgentClaude:   {"ANTHROPIC_API_KEY"},
	AgentCodex:    {"OPENAI_API_KEY"},
	AgentAgy:      {"GEMINI_API_KEY"},
	AgentCopilot:  {"GITHUB_TOKEN", "GH_TOKEN", "COPILOT_GITHUB_TOKEN"},
	AgentReasonix: {"DEEPSEEK_API_KEY"},
	AgentKimi:     {"KIMI_API_KEY"},
	AgentPool:     {"POOLSIDE_API_KEY", "POOLSIDE_API_URL"},
	AgentQwen:     {"DASHSCOPE_API_KEY", "BAILIAN_CODING_PLAN_API_KEY", "BAILIAN_TOKEN_PLAN_API_KEY"},
}

// envVarsForAgent returns the host environment variables forwarded into the
// sandbox for the given agent: the common non-secret set plus only the
// credential variables that agent needs.
func envVarsForAgent(agent Agent) []string {
	vars := append([]string{}, commonEnvVars...)
	return append(vars, agentEnvVars[agent]...)
}

// ensureFile creates an empty file (and its parent directories) if it does not
// already exist, returning true if the file exists afterward. Used to guarantee a
// bind source is present before mounting it.
func ensureFile(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(path, os.O_CREATE, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

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
		agentCmd = "copilot"
	case AgentReasonix:
		agentCmd = "reasonix"
	case AgentKimi:
		agentCmd = "kimi"
	case AgentPool:
		agentCmd = "pool"
	case AgentQwen:
		agentCmd = "qwen"
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
		// Fallback for kimi's default self-managed install location
		if opts.Agent == AgentKimi && hostAgentPath == "" {
			defaultPath := filepath.Join(hostHome, ".kimi-code", "bin", "kimi")
			if _, err := os.Stat(defaultPath); err == nil {
				hostAgentPath = defaultPath
			}
		}
		// Fallback for qwen's default install location
		if opts.Agent == AgentQwen && hostAgentPath == "" {
			defaultPath := filepath.Join(hostHome, ".local", "bin", "qwen")
			if _, err := os.Stat(defaultPath); err == nil {
				hostAgentPath = defaultPath
			}
		}
		// The current Copilot CLI installs as `copilot` in common setups, while
		// older integrations may still refer to `github-copilot-cli`.
		if opts.Agent == AgentCopilot && hostAgentPath == "" {
			if p, lookupErr := exec.LookPath("copilot"); lookupErr == nil {
				hostAgentPath = p
			} else {
				defaultPath := filepath.Join(hostHome, ".local", "bin", "copilot")
				if _, statErr := os.Stat(defaultPath); statErr == nil {
					hostAgentPath = defaultPath
				}
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

	// Bind-mount optional system paths if they exist. Deliberately NOT /var:
	// nothing an agent needs lives there, and mounting it would expose host
	// logs, spool, and other system state to the sandbox for no benefit.
	optPaths := []string{"/opt", "/etc/resolv.conf", "/etc/hosts", "/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/etc/alternatives", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf"}
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

	// Path to agy's keyring token inside the sandbox, if extracted. When set,
	// flar runs a private Secret Service serving only this token.
	var agySecretInSandbox string

	// Setup HOME directory structure inside sandbox
	bwrapArgs = append(bwrapArgs, "--dir", hostHome)

	// Bind-mount agent configurations into the home directory if prepared
	if opts.TempConfig != "" {
		switch opts.Agent {
		case AgentClaude:
			claudePath := filepath.Join(opts.TempConfig, ".claude")
			if _, err := os.Stat(claudePath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", claudePath, filepath.Join(hostHome, ".claude"))

				// Live-bind only THIS project's transcript directory from the host
				// (over the copied .claude), so sessions run in the sandbox are
				// written straight to disk and can be resumed. The bind is scoped to
				// one project's slug, so other projects' history stays invisible.
				slug := claudeProjectSlug(absProjectDir)
				hostProj := filepath.Join(hostHome, ".claude", "projects", slug)
				if err := os.MkdirAll(hostProj, 0o700); err == nil {
					bwrapArgs = append(bwrapArgs, "--bind", hostProj, hostProj)
				}
			}
			claudeJSONPath := filepath.Join(opts.TempConfig, ".claude.json")
			if _, err := os.Stat(claudeJSONPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", claudeJSONPath, filepath.Join(hostHome, ".claude.json"))
			}
		case AgentCodex:
			codexPath := filepath.Join(opts.TempConfig, ".codex")
			if _, err := os.Stat(codexPath); err == nil {
				store, err := prepareCodexStore(hostHome, absProjectDir, codexPath)
				if err != nil {
					return fmt.Errorf("prepare Codex store: %w", err)
				}
				bwrapArgs = append(bwrapArgs, "--bind", store, filepath.Join(hostHome, ".codex"))
			}
		case AgentAgy:
			agyPath := filepath.Join(opts.TempConfig, ".gemini")
			if _, err := os.Stat(agyPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", agyPath, filepath.Join(hostHome, ".gemini"))

				// Bind this workspace's private, scoped agy conversation store over
				// the copied config. Sessions created in the sandbox persist and can
				// be resumed with `agy --continue` / `--conversation`, while other
				// projects' conversations stay invisible. agy keeps every
				// conversation in one flat global store, so flar partitions it per
				// workspace here (see prepareAgyStore).
				if store, err := prepareAgyStore(hostHome, absProjectDir); err == nil {
					agyDir := filepath.Join(hostHome, ".gemini", "antigravity-cli")
					for _, sub := range agyStoreDirs {
						bwrapArgs = append(bwrapArgs, "--bind",
							filepath.Join(store, sub), filepath.Join(agyDir, sub))
					}
					bwrapArgs = append(bwrapArgs, "--bind",
						filepath.Join(store, "history.jsonl"),
						filepath.Join(agyDir, "history.jsonl"))
					bwrapArgs = append(bwrapArgs, "--bind",
						filepath.Join(store, "cache", "last_conversations.json"),
						filepath.Join(agyDir, "cache", "last_conversations.json"))
				}
			}
			secretPath := filepath.Join(opts.TempConfig, agySecretFile)
			if _, err := os.Stat(secretPath); err == nil {
				agySecretInSandbox = filepath.Join(hostHome, "."+agySecretFile)
				bwrapArgs = append(bwrapArgs, "--ro-bind", secretPath, agySecretInSandbox)
			}
		case AgentCopilot:
			copilotPath := filepath.Join(opts.TempConfig, ".copilot")
			if _, err := os.Stat(copilotPath); err == nil {
				store, err := prepareCopilotStore(hostHome, absProjectDir, copilotPath)
				if err != nil {
					return fmt.Errorf("prepare copilot store: %w", err)
				}
				bwrapArgs = append(bwrapArgs, "--bind", store, filepath.Join(hostHome, ".copilot"))
			}
			ghPath := filepath.Join(opts.TempConfig, "gh")
			if _, err := os.Stat(ghPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--dir", filepath.Join(hostHome, ".config"))
				bwrapArgs = append(bwrapArgs, "--bind", ghPath, filepath.Join(hostHome, ".config", "gh"))
			}
		case AgentReasonix:
			reasonixPath := filepath.Join(opts.TempConfig, ".reasonix")
			if _, err := os.Stat(reasonixPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", reasonixPath, filepath.Join(hostHome, ".reasonix"))

				// Live-bind only THIS project's session directory from the host
				// (over the copied .reasonix), so sessions run in the sandbox are
				// written straight to disk and can be resumed. Reasonix encodes
				// project paths the same way as Claude — replacing every
				// non-alphanumeric character with '-'.
				slug := claudeProjectSlug(absProjectDir)
				hostProj := filepath.Join(hostHome, ".reasonix", "projects", slug)
				if err := os.MkdirAll(hostProj, 0o700); err == nil {
					bwrapArgs = append(bwrapArgs, "--bind", hostProj, hostProj)
				}
			}
		case AgentKimi:
			kimiPath := filepath.Join(opts.TempConfig, ".kimi-code")
			if _, err := os.Stat(kimiPath); err == nil {
				// Kimi keeps resume state in global files (session_index.jsonl,
				// workspaces.json) that mix every project, so flar replaces the
				// whole home with a project-scoped shadow home seeded once from
				// the host (see prepareKimiStore). The kimi binary is not in the
				// store; it is bind-mounted read-only by the generic agent-binary
				// mount below.
				store, err := prepareKimiStore(hostHome, absProjectDir, kimiPath)
				if err != nil {
					return fmt.Errorf("prepare kimi store: %w", err)
				}
				bwrapArgs = append(bwrapArgs, "--bind", store, filepath.Join(hostHome, ".kimi-code"))

				// Kimi's OAuth access tokens live only ~15 minutes and the
				// refresh token rotates on every use, so a copied credential
				// goes stale almost immediately, and a sandbox-side refresh of
				// a copied token would invalidate the host's login (and vice
				// versa). Live-bind the host's credential dirs over the store's
				// copies instead: both sides then always see the latest tokens.
				// Exposure stays limited to Kimi's own OAuth tokens, which an
				// authenticated agent can read anyway.
				for _, sub := range []string{"credentials", "oauth"} {
					hostDir := filepath.Join(hostHome, ".kimi-code", sub)
					if dirExists(hostDir) {
						bwrapArgs = append(bwrapArgs, "--bind", hostDir, hostDir)
					}
				}
			}
		case AgentPool:
			// Pool keeps config (credentials, settings, skills) under
			// ~/.config/poolside and state (sessions, trajectories,
			// per-project prompt history/logs) under ~/.local/state/poolside.
			// The config is a temporary copy; the state is a per-project
			// shadow home forked once from the host so other projects'
			// sessions and trajectories never enter the sandbox.
			poolConfigPath := filepath.Join(opts.TempConfig, "poolside")
			if _, err := os.Stat(poolConfigPath); err == nil {
				configPath := poolConfigDir(hostHome)
				bwrapArgs = append(bwrapArgs, "--dir", filepath.Dir(configPath))
				bwrapArgs = append(bwrapArgs, "--bind", poolConfigPath, configPath)
			}

			// Prepare and bind the project-scoped shadow state. This is done
			// regardless of whether the config exists, since the user may
			// authenticate via POOLSIDE_API_KEY without a config directory.
			store, err := preparePoolStore(hostHome, absProjectDir)
			if err != nil {
				return fmt.Errorf("prepare pool store: %w", err)
			}
			statePath := poolStateDir(hostHome)
			bwrapArgs = append(bwrapArgs, "--dir", filepath.Dir(statePath))
			bwrapArgs = append(bwrapArgs, "--bind", store, statePath)
		case AgentQwen:
			qwenPath := filepath.Join(opts.TempConfig, ".qwen")
			if _, err := os.Stat(qwenPath); err == nil {
				bwrapArgs = append(bwrapArgs, "--bind", qwenPath, filepath.Join(hostHome, ".qwen"))

				// Live-bind only THIS project's directory from the host (over
				// the copied .qwen), so sessions run in the sandbox are written
				// straight to disk and can be resumed with `qwen --continue` /
				// `--resume`. Qwen encodes project paths the same way as Claude
				// — replacing every non-alphanumeric character with '-'.
				slug := claudeProjectSlug(absProjectDir)
				hostProj := filepath.Join(hostHome, ".qwen", "projects", slug)
				if err := os.MkdirAll(hostProj, 0o700); err == nil {
					bwrapArgs = append(bwrapArgs, "--bind", hostProj, hostProj)
				}
			}
		}

		// Git config
		gitConfigPath := filepath.Join(opts.TempConfig, ".gitconfig")
		if _, err := os.Stat(gitConfigPath); err == nil {
			bwrapArgs = append(bwrapArgs, "--bind", gitConfigPath, filepath.Join(hostHome, ".gitconfig"))
		}
	}

	// Mount the host flar binary inside the sandbox at its exact absolute path
	hostFlar, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get flar executable path: %w", err)
	}
	absHostFlar, err := filepath.Abs(hostFlar)
	if err != nil {
		absHostFlar = hostFlar
	}
	realHostFlar, err := filepath.EvalSymlinks(absHostFlar)
	if err != nil {
		realHostFlar = absHostFlar
	}

	flarDir := filepath.Dir(absHostFlar)
	var flarDirs []string
	currFlar := "/"
	for _, part := range strings.Split(flarDir, "/") {
		if part == "" {
			continue
		}
		currFlar = filepath.Join(currFlar, part)
		flarDirs = append(flarDirs, currFlar)
	}
	for _, d := range flarDirs {
		bwrapArgs = append(bwrapArgs, "--dir", d)
	}
	bwrapArgs = append(bwrapArgs, "--ro-bind", realHostFlar, absHostFlar)

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

	// Pass environment variables: the common non-secret set plus only the
	// credential variables the selected agent needs.
	bwrapArgs = append(bwrapArgs, "--setenv", "HOME", hostHome)
	for _, env := range envVarsForAgent(opts.Agent) {
		if val, exists := os.LookupEnv(env); exists {
			bwrapArgs = append(bwrapArgs, "--setenv", env, val)
		}
	}

	// Point agy at the private Secret Service and tell the internal service
	// where to read the token from.
	if agySecretInSandbox != "" {
		bwrapArgs = append(bwrapArgs,
			"--setenv", "DBUS_SESSION_BUS_ADDRESS", "unix:path="+agyBusSocket,
			"--setenv", "FLAR_AGY_SECRET_FILE", agySecretInSandbox,
		)
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
		agentArgs = append(agentArgs, hostAgentPath)
	case AgentReasonix:
		agentArgs = append(agentArgs, "reasonix")
		if !opts.AskMode {
			agentArgs = append(agentArgs, "--yolo")
		}
	case AgentKimi:
		// Use the resolved host path: kimi's install dir (~/.kimi-code/bin) is
		// not necessarily on PATH, but the binary is bind-mounted at exactly
		// this location inside the sandbox.
		agentArgs = append(agentArgs, hostAgentPath)
		// kimi refuses to combine --yolo (or --auto) with --prompt, so skip the
		// bypass flag for non-interactive runs; -ask forces omission entirely.
		if !opts.AskMode && !kimiPromptMode(opts.ExtraArgs) {
			agentArgs = append(agentArgs, "--yolo")
		}
	case AgentPool:
		agentArgs = append(agentArgs, "pool")
		// pool has no "dangerously skip permissions" flag; approvals are
		// governed by the ACP protocol and the user's pool settings.
	case AgentQwen:
		agentArgs = append(agentArgs, "qwen")
		if !opts.AskMode {
			agentArgs = append(agentArgs, "--yolo")
		}
	}

	if len(opts.ExtraArgs) > 0 {
		agentArgs = append(agentArgs, opts.ExtraArgs...)
	}

	// Prepare script inside sandbox
	var bashScript strings.Builder
	if opts.Network == "isolated" {
		// Run HTTP/HTTPS proxy inside sandbox using the absolute flar path
		bashScript.WriteString(fmt.Sprintf("%s --internal-proxy 9090 /run/flar-net/http-proxy.sock &\n", absHostFlar))
		// Run custom TCP proxies
		for _, port := range opts.AllowPorts {
			bashScript.WriteString(fmt.Sprintf("%s --internal-proxy %d /run/flar-net/port-%d.sock &\n", absHostFlar, port, port))
		}
		// Wait for the proxies to bind and start listening
		bashScript.WriteString("sleep 0.2\n")
	}

	// Launch the private Secret Service so agy can read its token from a socket
	// instead of the (absent) host keyring.
	if agySecretInSandbox != "" {
		bashScript.WriteString(fmt.Sprintf("%s --internal-secretsvc %s &\n", absHostFlar, agyBusSocket))
		bashScript.WriteString(fmt.Sprintf("for i in $(seq 1 50); do [ -S %s ] && break; sleep 0.02; done\n", agyBusSocket))
	}

	bashScript.WriteString("exec \"$@\"\n")

	// --chdir is an option, so it travels with the rest through --args below.
	bwrapArgs = append(bwrapArgs, "--chdir", absProjectDir)

	// The COMMAND and its args must stay on the real command line. bwrap only
	// consumes options from an --args fd; the trailing command is read from argv.
	commandArgs := []string{"/bin/bash", "-c", bashScript.String(), "flar" /* dummy $0 */}
	commandArgs = append(commandArgs, agentArgs...)

	if opts.Verbose {
		all := append(append([]string{}, bwrapArgs...), commandArgs...)
		fmt.Printf("Running command: bwrap %s\n", strings.Join(redactedArgs(all), " "))
	}

	// Pass the bwrap options through a pipe via --args instead of on the command
	// line. Otherwise the full mount layout (temp config paths, proxy socket,
	// bind list) and every --setenv value — including any ANTHROPIC_API_KEY,
	// GITHUB_TOKEN, etc. — show up in /proc/<pid>/cmdline, which the sandboxed
	// agent can read for PID 1. With --args, argv is just "bwrap --args 3 <cmd>".
	argsReader, argsWriter, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create args pipe: %w", err)
	}
	defer argsReader.Close()

	cmd := exec.Command("bwrap", append([]string{"--args", "3"}, commandArgs...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.ExtraFiles = []*os.File{argsReader} // becomes fd 3 in the child

	// Write in a goroutine so an argument blob larger than the pipe buffer can't
	// deadlock against bwrap reading it.
	writeErr := make(chan error, 1)
	go func() {
		_, err := argsWriter.Write(encodeBwrapArgs(bwrapArgs))
		if cerr := argsWriter.Close(); err == nil {
			err = cerr
		}
		writeErr <- err
	}()

	runErr := cmd.Run()
	if werr := <-writeErr; werr != nil && runErr == nil {
		return fmt.Errorf("failed to write bwrap args: %w", werr)
	}
	return runErr
}

// encodeBwrapArgs serializes arguments for bwrap's --args: each argument is
// nul-terminated (including the last, which bwrap requires).
func encodeBwrapArgs(args []string) []byte {
	var buf []byte
	for _, a := range args {
		buf = append(buf, a...)
		buf = append(buf, 0)
	}
	return buf
}

// verboseVisibleEnvVars are the --setenv variables whose values may appear in
// verbose output. Anything not listed — API keys, tokens, and any credential
// var added in the future — is redacted so `flar -v` cannot leak secrets into
// terminal scrollback or CI logs. The allowlist is deliberately fail-closed:
// new variables are hidden unless explicitly marked safe here.
var verboseVisibleEnvVars = map[string]bool{
	"HOME": true, "PATH": true, "TERM": true, "USER": true, "USERNAME": true,
	"LOGNAME": true, "XDG_CONFIG_HOME": true, "XDG_STATE_HOME": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "http_proxy": true, "https_proxy": true,
	"DBUS_SESSION_BUS_ADDRESS": true, "FLAR_AGY_SECRET_FILE": true,
}

// redactedArgs returns args with every --setenv value replaced by "<redacted>"
// unless the variable is in verboseVisibleEnvVars. Used for the verbose command
// dump, which would otherwise print every forwarded API key in clear text.
func redactedArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		out = append(out, args[i])
		if args[i] == "--setenv" && i+2 < len(args) {
			out = append(out, args[i+1])
			if verboseVisibleEnvVars[args[i+1]] {
				out = append(out, args[i+2])
			} else {
				out = append(out, "<redacted>")
			}
			i += 2
		}
	}
	return out
}
