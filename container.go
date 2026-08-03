package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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
	AgentMimo     Agent = "mimo"
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
	AgentMimo:     {"XIAOMI_API_KEY"},
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
	// ContainerCache persists images and layers built or pulled by podman
	// inside the sandbox under flar's own cache dir (see containerCacheDir).
	// When false, container storage lives on the sandbox's ephemeral tmpfs
	// and is discarded on exit.
	ContainerCache bool
}

// containerCacheDir returns the host directory where flar persists container
// images built or pulled inside the sandbox when container caching is
// enabled. It is deliberately NOT podman's usual ~/.local/share/containers
// location, so flar's cache stays separate from any host-side podman use and
// it is obvious which directory the sandbox may write to. Respects
// XDG_CACHE_HOME.
func containerCacheDir(home string) string {
	if cacheHome := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(cacheHome) {
		return filepath.Join(cacheHome, "flar", "containers")
	}
	return filepath.Join(home, ".cache", "flar", "containers")
}

// containerSupportArgs writes the configuration rootless podman needs into
// tempConfig and returns the bwrap arguments that mount it into the sandbox.
// hostEtcContainers is the host's /etc/containers path when it exists, or ""
// when it does not.
//
// bwrap maps only a single UID into the sandbox, so podman runs in "single
// mapping" mode. The overlay driver needs ignore_chown_errors to unpack
// layers in that mode, and empty /etc/subuid + /etc/subgid make podman pick
// single mapping deterministically instead of attempting (and failing) to map
// subordinate IDs that don't exist in the namespace.
//
// podman 5 refuses to pull images without a policy.json. When the host ships
// /etc/containers it is copied into tempConfig and mounted back read-only,
// inheriting the host's trust policy (e.g. signature verification) and
// registry configuration. When the host ships no policy, flar deliberately
// does NOT generate one — see the note at the policy mount below — and
// RunSandbox warns that image pulls will not work until the host provides
// /etc/containers/policy.json.
//
// flar's storage.conf is mounted at $HOME/.config/containers/storage.conf
// rather than /etc/containers: rootless podman overrides graphroot/runroot
// from system-wide configs with per-user defaults, so only the user-level
// file is authoritative for the storage location.
//
// When containerCache is true, image storage (graphroot) is bind-mounted
// read-write from containerCacheDir so images survive across runs. Otherwise
// graphroot points at the sandbox's ephemeral tmpfs home and everything is
// discarded on exit.
func containerSupportArgs(tempConfig, hostHome string, uid int, containerCache bool, hostEtcContainers string) ([]string, error) {
	podmanDir := filepath.Join(tempConfig, "podman")
	etcContainers := filepath.Join(podmanDir, "etc-containers")
	if hostEtcContainers != "" {
		if err := CopyDir(hostEtcContainers, etcContainers); err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(etcContainers, 0o700); err != nil {
		return nil, err
	}

	var args []string

	graphroot := filepath.Join(hostHome, ".local", "share", "containers", "storage")
	if containerCache {
		graphroot = containerCacheDir(hostHome)
		if err := os.MkdirAll(graphroot, 0o700); err != nil {
			return nil, fmt.Errorf("create container cache dir: %w", err)
		}
		args = append(args, "--bind", graphroot, graphroot)
	}

	storageConf := fmt.Sprintf(`[storage]
driver = "overlay"
graphroot = %q
runroot = %q

[storage.options.overlay]
ignore_chown_errors = "true"
`, graphroot, fmt.Sprintf("/run/user/%d/containers", uid))
	storageConfPath := filepath.Join(podmanDir, "storage.conf")
	if err := os.WriteFile(storageConfPath, []byte(storageConf), 0o600); err != nil {
		return nil, err
	}
	homeCfgDir := filepath.Join(hostHome, ".config", "containers")
	args = append(args,
		"--dir", homeCfgDir,
		"--ro-bind", storageConfPath, filepath.Join(homeCfgDir, "storage.conf"),
		// podman's image-copy staging dir defaults to /var/tmp, and the
		// build-commit path uses that default even though TMPDIR is set;
		// /var does not exist in the sandbox, so provide it on the tmpfs.
		"--dir", "/var/tmp",
	)

	// If the copy has no policy.json (the host ships none), flar does not
	// generate one: the only generatable policy is insecureAcceptAnything,
	// which would silently disable image signature verification. A security
	// tool should not author that posture on the user's behalf, so image
	// pulls fail closed (with podman's own missing-policy error) until the
	// host provides a policy. RunSandbox prints the actionable warning.
	args = append(args, "--ro-bind", etcContainers, "/etc/containers")

	// Empty subuid/subgid: the single-UID mapping can never honor real
	// ranges, and binding the host's files (which list them) would make
	// podman attempt the mapping and fail. Empty files make podman pick the
	// single-mapping path cleanly; with no files at all it logs an error at
	// every invocation.
	for _, name := range []string{"subuid", "subgid"} {
		p := filepath.Join(podmanDir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--ro-bind", p, "/etc/"+name)
	}

	return args, nil
}

// sandboxAgentScript builds the script run inside the sandbox: flar's helper
// daemons (network proxies, agy's private Secret Service) followed by the
// agent itself.
//
// The agent is run as a monitored child rather than exec'd: its exit status
// is written to fd 4 and the pipe is then closed, so flar learns the agent
// has exited even when orphaned processes keep the sandbox alive. Without
// this, bwrap's monitor adopts the orphans and waits for them forever,
// hanging flar. Two known orphan sources: podman's rootless pause process
// (catatonit -P), which survives any podman use inside the sandbox, and
// flar's own helper daemons in isolated network mode. Every daemon closes
// fd 4 so none of them can hold the pipe open.
//
// Termination signals cannot be delivered into the sandbox's user namespace
// from the host — group-directed kills silently skip it, and bwrap does not
// forward what it receives — so flar signals "stop now" by writing to the
// control pipe on fd 5. The watcher subshell reads it and sends TERM to the
// sandbox's whole process group from the inside. The script traps TERM/INT/
// HUP rather than ignoring them (an ignored signal stays ignored across
// exec, which would make the agent permanently deaf to it) so it survives
// long enough to report the agent's resulting exit status on fd 4.
func sandboxAgentScript(opts RunOpts, absHostFlar, agySecretInSandbox string) string {
	var s strings.Builder
	if opts.Network == "isolated" {
		// Run HTTP/HTTPS proxy inside sandbox using the absolute flar path
		s.WriteString(fmt.Sprintf("%s --internal-proxy 9090 /run/flar-net/http-proxy.sock 4>&- 5>&- &\n", absHostFlar))
		// Run custom TCP proxies
		for _, port := range opts.AllowPorts {
			s.WriteString(fmt.Sprintf("%s --internal-proxy %d /run/flar-net/port-%d.sock 4>&- 5>&- &\n", absHostFlar, port, port))
		}
		// Wait for the proxies to bind and start listening
		s.WriteString("sleep 0.2\n")
	}

	// Launch the private Secret Service so agy can read its token from a socket
	// instead of the (absent) host keyring.
	if agySecretInSandbox != "" {
		s.WriteString(fmt.Sprintf("%s --internal-secretsvc %s 4>&- 5>&- &\n", absHostFlar, agyBusSocket))
		s.WriteString(fmt.Sprintf("for i in $(seq 1 50); do [ -S %s ] && break; sleep 0.02; done\n", agyBusSocket))
	}

	// The agent runs in its own session/process group (setsid) so it can be
	// signalled as a tree without touching anything else in the sandbox:
	// kill -TERM 0 would also reach bwrap's own monitor process — it shares
	// the sandbox's process group and sits inside the user namespace — and
	// bwrap tears the whole sandbox down the moment its monitor is
	// signalled, killing the wrapper before it can report.
	s.WriteString("setsid \"$@\" 4>&- 5>&- &\n")
	s.WriteString("agent_pid=$!\n")
	// Control-channel watcher: a byte from flar on fd 5 means "stop now";
	// TERM the agent's process group. EOF (flar exited normally) makes read
	// return non-zero: kill nothing. The wrapper then reports the agent's
	// resulting exit status on fd 4 via the plain wait below.
	s.WriteString("( if read -r _ <&5 2>/dev/null; then kill -TERM -- -\"$agent_pid\" 2>/dev/null; fi ) 4>&- &\n")
	// Safety net in case the wrapper itself is signalled: survive long
	// enough to report the agent's status. (An ignored signal would stay
	// ignored across exec and make the agent deaf to it, so trap, never
	// ignore.)
	s.WriteString("trap 'if [ -n \"$agent_pid\" ]; then wait \"$agent_pid\"; rc=$?; else rc=130; fi; printf '%s' \"$rc\" >&4 2>/dev/null; exit \"$rc\"' INT TERM HUP\n")
	s.WriteString("wait $agent_pid\n")
	s.WriteString("rc=$?\n")
	s.WriteString("printf '%s' \"$rc\" >&4 2>/dev/null\n")
	s.WriteString("exit $rc\n")
	return s.String()
}

// RunSandbox runs the Bubblewrap sandbox with the specified options. It
// returns the agent's exit code; a non-nil error means the sandbox itself
// failed (bwrap could not start, or the agent never reported).
func RunSandbox(opts RunOpts) (int, error) {
	hostHome, err := os.UserHomeDir()
	if err != nil {
		return 0, fmt.Errorf("failed to get user home directory: %w", err)
	}

	absProjectDir, err := filepath.Abs(opts.ProjectDir)
	if err != nil {
		return 0, fmt.Errorf("failed to resolve absolute project path: %w", err)
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
	case AgentMimo:
		agentCmd = "mimo"
	default:
		return 0, fmt.Errorf("unknown or unsupported agent: %s", opts.Agent)
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
		// Fallback for mimo's default install location (~/.mimocode/bin/mimo)
		if opts.Agent == AgentMimo && hostAgentPath == "" {
			defaultPath := filepath.Join(hostHome, ".mimocode", "bin", "mimo")
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
			return 0, fmt.Errorf("agent binary %q not found on host; please ensure it is in your PATH", agentCmd)
		}
	}

	realAgentPath, err := filepath.EvalSymlinks(hostAgentPath)
	if err != nil {
		realAgentPath = hostAgentPath
	}

	// Prepare bubblewrap arguments
	bwrapArgs := []string{
		"--unshare-all",
		// If flar itself dies, kill the whole sandbox rather than leaving it
		// running unattended.
		"--die-with-parent",
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

	// Prepare the sandbox for rootless podman/buildah (image pulls, container
	// builds, container runs). Empirically required:
	//   - podman lstats /run/user/<uid> at startup and aborts if absent
	//   - containers/image uses TMPDIR (default /var/tmp), which doesn't exist here
	//   - podman hard-errors if /sys/fs/cgroup is missing entirely; a real
	//     cgroup2 mount is impossible from inside the userns (bwrap drops all
	//     capabilities before exec), so an empty dir is the achievable state
	//     and podman degrades gracefully to no-cgroup operation.
	uid := os.Getuid()
	runtimeDir := fmt.Sprintf("/run/user/%d", uid)
	bwrapArgs = append(bwrapArgs,
		"--dir", runtimeDir,
		"--setenv", "XDG_RUNTIME_DIR", runtimeDir,
		"--setenv", "TMPDIR", "/tmp",
		"--dir", "/sys/fs/cgroup",
		"--setenv", "PODMAN_IGNORE_CGROUPSV1_WARNING", "1",
	)
	// The config files podman 5 additionally requires (policy.json, a
	// storage.conf tuned for bwrap's single-UID mapping, empty subuid/subgid).
	if opts.TempConfig != "" {
		hostEtcContainers := ""
		if dirExists("/etc/containers") {
			hostEtcContainers = "/etc/containers"
		}
		if !fileExists("/etc/containers/policy.json") {
			fmt.Fprintf(os.Stderr, "Warning: host has no /etc/containers/policy.json. flar will not generate an accept-anything image signature policy, so container image pulls inside the sandbox will not work until the host provides one (usually shipped by the containers-common package).\n")
		}
		supportArgs, err := containerSupportArgs(opts.TempConfig, hostHome, uid, opts.ContainerCache, hostEtcContainers)
		if err != nil {
			return 0, fmt.Errorf("prepare container support: %w", err)
		}
		bwrapArgs = append(bwrapArgs, supportArgs...)
	}

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
					return 0, fmt.Errorf("prepare Codex store: %w", err)
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
					return 0, fmt.Errorf("prepare copilot store: %w", err)
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
					return 0, fmt.Errorf("prepare kimi store: %w", err)
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
				return 0, fmt.Errorf("prepare pool store: %w", err)
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
		case AgentMimo:
			// mimo's config dir (~/.config/mimocode/) holds user settings; the
			// temp copy was prepared by PrepareConfigDir.
			mimoCfgPath := filepath.Join(opts.TempConfig, "mimocode-config")
			if _, err := os.Stat(mimoCfgPath); err == nil {
				cfgDir := mimoConfigDir(hostHome)
				bwrapArgs = append(bwrapArgs, "--dir", filepath.Dir(cfgDir))
				bwrapArgs = append(bwrapArgs, "--bind", mimoCfgPath, cfgDir)
			}

			// mimo keeps all sessions in a single global SQLite database that
			// mixes every project. flar forks it per project into a shadow home
			// so other projects' sessions stay invisible (see prepareMimoStore).
			// The filtered data dir copy (auth.json, skills, etc.) from
			// PrepareConfigDir is merged into the store on first seed.
			mimoDataSrc := filepath.Join(opts.TempConfig, "mimocode-data")
			store, err := prepareMimoStore(hostHome, absProjectDir, mimoDataSrc)
			if err != nil {
				return 0, fmt.Errorf("prepare mimo store: %w", err)
			}
			mimoData := mimoDataDir(hostHome)
			bwrapArgs = append(bwrapArgs, "--dir", filepath.Dir(mimoData))
			bwrapArgs = append(bwrapArgs, "--bind", store, mimoData)

			// Live-bind the host's memory/projects/<uuid>/ directory so memory
			// written inside the sandbox persists to the host. The project UUID
			// comes from the shadow database.
			if projMemDir := mimoProjectMemoryDir(store, absProjectDir); projMemDir != "" {
				hostProjMem := filepath.Join(mimoData, "memory", "projects", filepath.Base(projMemDir))
				if err := os.MkdirAll(hostProjMem, 0o700); err == nil {
					bwrapArgs = append(bwrapArgs, "--bind", hostProjMem, hostProjMem)
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
		return 0, fmt.Errorf("failed to get flar executable path: %w", err)
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

	// Qwen's ~/.local/bin/qwen is a wrapper script that execs the real
	// installation at ~/.local/lib/qwen-code/ (node runtime + cli-entry.js).
	// Mount that directory so the wrapper can find its target inside the sandbox.
	if opts.Agent == AgentQwen {
		qwenLibDir := filepath.Join(hostHome, ".local", "lib", "qwen-code")
		if dirExists(qwenLibDir) {
			bwrapArgs = append(bwrapArgs, "--dir", filepath.Join(hostHome, ".local", "lib"))
			bwrapArgs = append(bwrapArgs, "--ro-bind", qwenLibDir, qwenLibDir)
		}
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
		// Use the resolved host path: qwen's install dir (~/.local/bin) is
		// not necessarily on PATH, but the binary is bind-mounted at exactly
		// this location inside the sandbox.
		agentArgs = append(agentArgs, hostAgentPath)
		if !opts.AskMode {
			agentArgs = append(agentArgs, "--yolo")
		}
	case AgentMimo:
		// Use the resolved host path: mimo's install dir (~/.mimocode/bin) is
		// not necessarily on PATH, but the binary is bind-mounted at exactly
		// this location inside the sandbox.
		agentArgs = append(agentArgs, hostAgentPath)
		if !opts.AskMode {
			agentArgs = append(agentArgs, "--dangerously-skip-permissions")
		}
		// mimo needs --trust to skip the workspace trust prompt inside the
		// sandbox, since the project directory is bind-mounted.
		agentArgs = append(agentArgs, "--trust")
	}

	if len(opts.ExtraArgs) > 0 {
		agentArgs = append(agentArgs, opts.ExtraArgs...)
	}

	bashScript := sandboxAgentScript(opts, absHostFlar, agySecretInSandbox)

	// --chdir is an option, so it travels with the rest through --args below.
	bwrapArgs = append(bwrapArgs, "--chdir", absProjectDir)

	// The COMMAND and its args must stay on the real command line. bwrap only
	// consumes options from an --args fd; the trailing command is read from argv.
	commandArgs := []string{"/bin/bash", "-c", bashScript, "flar" /* dummy $0 */}
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
		return 0, fmt.Errorf("failed to create args pipe: %w", err)
	}
	defer argsReader.Close()

	// The sandbox script reports the agent's exit status through fd 4 and
	// then exits, closing the pipe. Reading EOF there means "the agent is
	// done" regardless of orphaned processes still alive in the sandbox
	// (podman's catatonit pause process, flar's own proxies, daemons the
	// agent left behind) — orphans get adopted by bwrap's monitor, which
	// waits for them forever, so flar must not wait for bwrap to exit on
	// its own.
	statusReader, statusWriter, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("failed to create status pipe: %w", err)
	}
	// Control channel: writing to it tells the sandbox script to terminate
	// the agent (see sandboxAgentScript). Host-side signals cannot cross
	// into the sandbox's user namespace directly.
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		statusReader.Close()
		statusWriter.Close()
		return 0, fmt.Errorf("failed to create control pipe: %w", err)
	}

	cmd := exec.Command("bwrap", append([]string{"--args", "3"}, commandArgs...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.ExtraFiles = []*os.File{argsReader, statusWriter, controlReader} // fds 3, 4 and 5 in the child
	// Run the sandbox in its own process group so flar can kill the entire
	// tree at once once the agent has reported.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		statusReader.Close()
		statusWriter.Close()
		controlReader.Close()
		controlWriter.Close()
		return 0, fmt.Errorf("failed to start bwrap: %w", err)
	}
	// bwrap owns these ends now; flar keeps statusReader and controlWriter.
	statusWriter.Close()
	controlReader.Close()

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

	// Deliver termination signals over the control pipe: the sandbox's user
	// namespace is unreachable for host-side signals (group-directed kills
	// silently skip it), so the script's watcher does the killing from the
	// inside. A second signal escalates to SIGKILL, which tears the sandbox
	// down by killing bwrap itself.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	var escalated atomic.Bool
	go func() {
		for range sigs {
			if !escalated.Swap(true) {
				// A full line: the sandbox-side read blocks until a newline.
				_, _ = controlWriter.Write([]byte("stop\n"))
			} else {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}()

	// Block until the script reports the agent's exit status and closes fd 4.
	rcData, _ := io.ReadAll(statusReader)
	statusReader.Close()
	controlWriter.Close()

	// Kill whatever the sandbox still holds: orphans keep bwrap alive, so
	// tear down the whole group instead of waiting for it.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	waitErr := cmd.Wait()

	if werr := <-writeErr; werr != nil && len(rcData) == 0 && waitErr == nil {
		return 0, fmt.Errorf("failed to write bwrap args: %w", werr)
	}
	if rc, perr := strconv.Atoi(strings.TrimSpace(string(rcData))); perr == nil {
		return rc, nil
	}
	// The user forced teardown before the script could report.
	if escalated.Load() {
		return 137, nil
	}
	// The script never reported: bwrap failed before the agent ran.
	if waitErr != nil {
		return 0, waitErr
	}
	return 0, fmt.Errorf("sandbox exited without reporting the agent's exit status")
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
	"XDG_RUNTIME_DIR": true, "TMPDIR": true, "PODMAN_IGNORE_CGROUPSV1_WARNING": true,
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
