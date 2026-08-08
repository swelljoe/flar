package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
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
	AgentOmp      Agent = "omp"
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
	AgentOmp:      {},
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

// opensslConfCandidates lists the paths where a distribution's OpenSSL build
// keeps its master configuration file. OpenSSL compiles OPENSSLDIR in, so the
// location varies: Fedora and RHEL use /etc/pki/tls, Debian and Ubuntu use
// /usr/lib/ssl with /etc/ssl symlinked in, Arch and Alpine use /etc/ssl, and
// source builds default to /usr/local/ssl. All of them are probed rather than
// stopping at the first hit, because a host can carry more than one OpenSSL
// and the agent inside the sandbox may link against either.
var opensslConfCandidates = []string{
	"/etc/ssl/openssl.cnf",
	"/etc/pki/tls/openssl.cnf",
	"/etc/openssl/openssl.cnf",
	"/usr/lib/ssl/openssl.cnf",
	"/usr/local/ssl/openssl.cnf",
}

// maxOpensslIncludeDepth bounds how far opensslIncludePaths follows nested
// .include directives. Real configurations nest one or two levels; the limit
// only exists so a pathological chain cannot spin.
const maxOpensslIncludeDepth = 8

// opensslIncludePaths returns the host paths that the system OpenSSL
// configuration pulls in with .include directives, so they can be bound into
// the sandbox. A missing include target is fatal to OpenSSL initialization,
// not a warning: on Fedora the stock /etc/pki/tls/openssl.cnf includes
// /etc/crypto-policies/back-ends/opensslcnf.config, and without it every
// OpenSSL consumer in the sandbox dies at startup with "OpenSSL configuration
// error ... calling stat(...)" — which is what makes node unusable there.
//
// The targets are discovered by reading the host's own configuration rather
// than hardcoding the crypto-policies layout, since the set of includes (and
// whether there are any at all) differs across distributions.
//
// confPaths are the configuration files to scan; ones that do not exist are
// skipped. bound lists paths already bind-mounted into the sandbox, whose
// contents therefore need no separate mount. Returned paths are absolute,
// deduplicated, and exist on the host.
func opensslIncludePaths(confPaths, bound []string) []string {
	var out []string
	added := make(map[string]bool)
	visited := make(map[string]bool)
	covered := append([]string{}, bound...)

	add := func(p string) {
		if added[p] || pathCovered(p, covered) {
			return
		}
		added[p] = true
		out = append(out, p)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			covered = append(covered, p)
		}
	}

	// walk parses one configuration file, or every entry of a configuration
	// directory, and records what it includes. It stats through symlinks
	// because bwrap's --ro-bind resolves the source path the same way: on
	// Fedora the include target is a symlink into /usr/share/crypto-policies,
	// and binding it at its /etc path is exactly what OpenSSL then finds.
	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		if depth > maxOpensslIncludeDepth || visited[path] {
			return
		}
		visited[path] = true

		info, err := os.Stat(path)
		if err != nil {
			return
		}
		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				return
			}
			for _, e := range entries {
				walk(filepath.Join(path, e.Name()), depth+1)
			}
			return
		}

		for _, inc := range parseOpensslIncludes(path) {
			if !filepath.IsAbs(inc) {
				// OpenSSL resolves a relative include against the directory of
				// the file that names it.
				inc = filepath.Join(filepath.Dir(path), inc)
			}
			inc = filepath.Clean(inc)
			if _, err := os.Stat(inc); err != nil {
				// Nothing to bind, and OpenSSL will fail on it identically
				// inside and outside the sandbox. Not flar's problem to fix.
				continue
			}
			add(inc)
			walk(inc, depth+1)
		}
	}

	for _, conf := range confPaths {
		if _, err := os.Stat(conf); err != nil {
			continue
		}
		// The configuration file itself has to be reachable too; on the usual
		// layouts it already lives under a bound path and add() drops it.
		add(conf)
		walk(conf, 0)
	}
	return out
}

// parseOpensslIncludes extracts the targets of the .include directives in an
// OpenSSL configuration file. Both accepted spellings are handled:
//
//	.include /path/to/file
//	.include = /path/to/file
//
// Targets containing $ are skipped: OpenSSL expands variables and $ENV:: at
// load time against state flar cannot reproduce here, so guessing at the path
// would be worse than leaving it alone.
func parseOpensslIncludes(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, ".include")
		if !ok {
			continue
		}
		// A separator must follow, so a section named e.g. `.includes` is not
		// mistaken for the directive.
		if rest == "" || !strings.ContainsAny(rest[:1], " \t=") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "="))
		if i := strings.Index(val, "#"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		val = strings.Trim(val, `"'`)
		if val == "" || strings.Contains(val, "$") {
			continue
		}
		out = append(out, val)
	}
	return out
}

// pathCovered reports whether path is already inside one of roots, either
// because it is a root itself or because a root is an ancestor directory.
func pathCovered(path string, roots []string) bool {
	for _, root := range roots {
		root = strings.TrimSuffix(root, "/")
		if root == "" {
			continue
		}
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
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

// tiocstiSeccompFilter returns a classic-BPF seccomp program (the raw
// struct sock_filter sequence bwrap --seccomp expects) that denies exactly
// one syscall: ioctl(2) with request TIOCSTI (0x5412), which pushes bytes
// into the terminal's input queue as if the user had typed them.
//
// The sandboxed agent now runs in flar's own session and in the terminal's
// foreground process group — that is what makes it a transparent, fully
// interactive wrapper — so without this filter a compromised agent could
// inject keystrokes into the terminal and have them executed by the user's
// shell outside the sandbox (bubblewrap's documented CVE-2017-5226
// warning). Nothing a TUI legitimately does needs TIOCSTI, so denying it
// costs nothing interactively.
//
// The filter is arch-checked so it is safe everywhere, and denies TIOCSTI
// where the ioctl syscall number is known (x86_64: 16, aarch64: 29). On any
// other architecture it allows everything, so the sandbox behaves exactly
// like flar without the filter rather than risking a too-broad denial.
//
// bwrap fails closed: if the kernel refuses to load the filter, its
// prctl(PR_SET_SECCOMP) fails and bwrap dies before the sandbox starts.
func tiocstiSeccompFilter() []byte {
	// Classic BPF (struct sock_filter): code | jt | jf | k, 8 bytes each.
	//   BPF_LD|BPF_W|BPF_ABS (0x20): A = 32-bit word at seccomp_data offset k
	//   BPF_JMP|BPF_JEQ|BPF_K (0x15): if A == k jump jt (else jf) instructions
	//   BPF_RET|BPF_K (0x06): return k (SECCOMP_RET_*)
	// seccomp_data offsets: nr=0, arch=4, args[0]=16, args[1]=24.
	const (
		opLD   = 0x20
		opJEQ  = 0x15
		opRET  = 0x06
		retAll = 0x7fff0000     // SECCOMP_RET_ALLOW
		retErr = 0x00050000 | 1 // SECCOMP_RET_ERRNO | EPERM
		archX  = 0xc000003e     // AUDIT_ARCH_X86_64
		archA  = 0xc00000b7     // AUDIT_ARCH_AARCH64
	)
	insn := func(code uint16, jt, jf uint8, k uint32) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint16(b[0:2], code)
		b[2], b[3] = jt, jf
		binary.LittleEndian.PutUint32(b[4:8], k)
		return b
	}
	// Instruction layout (offsets are pc+1+jt/jf):
	//   0  ld  arch
	//   1  jeq x86_64  -> 5
	//   2  jeq aarch64 -> 3, else -> 10 (allow)
	//   3  ld  nr
	//   4  jeq 29 (aarch64 ioctl) -> 7, else -> 10
	//   5  ld  nr
	//   6  jeq 16 (x86_64 ioctl) -> 7, else -> 10
	//   7  ld  args[1] (ioctl request)
	//   8  jeq TIOCSTI -> 9, else -> 10
	//   9  ret EPERM
	//   10 ret ALLOW
	var f []byte
	for _, in := range [][]byte{
		insn(opLD, 0, 0, 4),
		insn(opJEQ, 3, 0, archX),
		insn(opJEQ, 0, 7, archA),
		insn(opLD, 0, 0, 0),
		insn(opJEQ, 2, 5, 29),
		insn(opLD, 0, 0, 0),
		insn(opJEQ, 0, 3, 16),
		insn(opLD, 0, 0, 24),
		insn(opJEQ, 0, 1, 0x5412),
		insn(opRET, 0, 0, retErr),
		insn(opRET, 0, 0, retAll),
	} {
		f = append(f, in...)
	}
	return f
}

// sandboxAgentScript builds the script run inside the sandbox: flar's helper
// daemons (network proxies, agy's private Secret Service) started in the
// background, then the agent exec'd in the foreground.
//
// The exec is what keeps the agent fully interactive: it inherits flar's
// stdin/stdout/stderr, flar's session (so it owns the controlling terminal
// and can open /dev/tty), and flar's process group (so tty reads never hit
// SIGTTIN, and Ctrl-C reaches it straight from the kernel). flar starts
// bwrap without Setpgid for the same reason.
//
// Process lifetime needs no machinery here. When the agent exits, the
// sandbox's pid 1 (bwrap's reaper) immediately reports its exit status to
// bwrap's host-side monitor over an eventfd, and the monitor exits with
// that status — it does NOT wait for stray processes still alive in the
// sandbox. --die-with-parent then SIGKILLs pid 1, collapsing the pid
// namespace and everything left in it: podman's rootless pause process
// (catatonit -P), flar's own daemons above, anything the agent left
// running. So flar just waits for bwrap and reads the agent's exit status
// from it.
func sandboxAgentScript(opts RunOpts, absHostFlar, agySecretInSandbox string) string {
	var s strings.Builder
	if opts.Network == "isolated" {
		// Run HTTP/HTTPS proxy inside sandbox using the absolute flar path
		s.WriteString(fmt.Sprintf("%s --internal-proxy 9090 /run/flar-net/http-proxy.sock &\n", absHostFlar))
		// Run custom TCP proxies
		for _, port := range opts.AllowPorts {
			s.WriteString(fmt.Sprintf("%s --internal-proxy %d /run/flar-net/port-%d.sock &\n", absHostFlar, port, port))
		}
		// Wait for the proxies to bind and start listening
		s.WriteString("sleep 0.2\n")
	}

	// Launch the private Secret Service so agy can read its token from a socket
	// instead of the (absent) host keyring.
	if agySecretInSandbox != "" {
		s.WriteString(fmt.Sprintf("%s --internal-secretsvc %s &\n", absHostFlar, agyBusSocket))
		s.WriteString(fmt.Sprintf("for i in $(seq 1 50); do [ -S %s ] && break; sleep 0.02; done\n", agyBusSocket))
	}

	s.WriteString("exec \"$@\"\n")
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
	case AgentOmp:
		agentCmd = "omp"



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
		// Fallback for omp's default install location (~/.local/bin/omp or
		// the bun-managed global install directory).
		if opts.Agent == AgentOmp && hostAgentPath == "" {
			for _, d := range []string{
				filepath.Join(hostHome, ".local", "bin", "omp"),
				filepath.Join(hostHome, ".bun", "bin", "omp"),
			} {
				if _, err := os.Stat(d); err == nil {
					hostAgentPath = d
					break
				}
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
		// Ties the sandbox's lifetime to bwrap's host-side monitor process:
		// when the monitor exits (the agent finished) or is killed (flar
		// died, or signal escalation below), the sandbox's pid 1 is
		// SIGKILLed and the kernel tears down the whole pid namespace,
		// including any orphans still alive in it (podman's pause process,
		// flar's proxy daemons).
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

	// The system OpenSSL configuration can .include files that live outside
	// every path bound above — on Fedora and RHEL it includes the
	// crypto-policies back-end under /etc/crypto-policies. OpenSSL treats a
	// missing include as a hard error, so without these the sandbox has no
	// working TLS at all and node in particular refuses to start. Mounted
	// after optPaths so an include nested inside one of those trees layers
	// over it rather than being hidden by it.
	alreadyBound := append([]string{"/usr"}, optPaths...)
	for _, p := range opensslIncludePaths(opensslConfCandidates, alreadyBound) {
		bwrapArgs = append(bwrapArgs, "--ro-bind-try", p, p)
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
	case AgentOmp:
		// Use the resolved host path: omp is typically in PATH but fall back
		// to common install locations via the fallback above.
		agentArgs = append(agentArgs, hostAgentPath)
		if !opts.AskMode {
			agentArgs = append(agentArgs, "--dangerously-skip-permissions")
		}
	}

	if len(opts.ExtraArgs) > 0 {
		agentArgs = append(agentArgs, opts.ExtraArgs...)
	}

	bashScript := sandboxAgentScript(opts, absHostFlar, agySecretInSandbox)

	// --chdir is an option, so it travels with the rest through --args below.
	bwrapArgs = append(bwrapArgs, "--chdir", absProjectDir)
	// Deny the TIOCSTI ioctl inside the sandbox (see tiocstiSeccompFilter).
	// The filter program is fed to bwrap on fd 4, created below; bwrap dies
	// rather than start without the filter, so any filter-load failure fails
	// the sandbox closed.
	bwrapArgs = append(bwrapArgs, "--seccomp", "4")

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

	// The TIOCSTI-denial filter (see tiocstiSeccompFilter) travels on fd 4:
	// bwrap reads it to EOF during option parsing, before the sandbox is
	// unshared. 88 bytes always fit a pipe buffer, so writing here cannot
	// block.
	seccompReader, seccompWriter, err := os.Pipe()
	if err != nil {
		argsWriter.Close()
		return 0, fmt.Errorf("failed to create seccomp pipe: %w", err)
	}
	if _, err := seccompWriter.Write(tiocstiSeccompFilter()); err != nil {
		argsWriter.Close()
		seccompReader.Close()
		seccompWriter.Close()
		return 0, fmt.Errorf("failed to write seccomp filter: %w", err)
	}
	seccompWriter.Close()

	cmd := exec.Command("bwrap", append([]string{"--args", "3"}, commandArgs...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.ExtraFiles = []*os.File{argsReader, seccompReader} // fds 3 and 4 in the child
	// Deliberately NO Setpgid here: the sandbox must stay in the terminal's
	// foreground process group, or tty reads from the agent would hit
	// SIGTTIN (background group) and every interactive TUI would break.
	// Sharing the foreground group is also what delivers Ctrl-C straight to
	// the agent with no forwarding: the kernel signals the whole group, the
	// agent handles it, and bwrap's monitor survives SIGINT.

	if err := cmd.Start(); err != nil {
		argsWriter.Close()
		seccompReader.Close()
		return 0, fmt.Errorf("failed to start bwrap: %w", err)
	}
	seccompReader.Close()

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

	// Find the agent's pid as the host sees it, for signal forwarding. bwrap
	// forks twice: cmd.Process is the host-side monitor, its child is the
	// sandbox's pid 1 (bwrap's reaper), and pid 1's child is the agent. The
	// lookup runs once, right after start: at that point the agent is pid
	// 1's only child, whereas later on orphans the agent leaves behind
	// (podman's pause process) get reparented to pid 1 and would make the
	// answer ambiguous.
	var agentPid atomic.Int64
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		init, err := firstChild(cmd.Process.Pid, deadline)
		if err != nil {
			return
		}
		agent, err := firstChild(init, deadline)
		if err != nil {
			return
		}
		agentPid.Store(int64(agent))
	}()

	// Forward termination signals to the agent by pid: kill(2) permission is
	// uid-based, so the sandbox's namespaces don't block it (only the
	// sandbox's pid 1 is signal-protected, as every pid-namespace init is).
	// Ctrl-C needs no forwarding — the agent sits in the terminal's
	// foreground group and receives it directly — but SIGINT is included so
	// a signal aimed at flar alone still stops the agent. A second signal,
	// or a first one before the agent's pid is known, gives up on graceful
	// shutdown and kills bwrap's monitor, which takes the whole sandbox with
	// it (--die-with-parent).
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	var escalated atomic.Bool
	go func() {
		for range sigs {
			pid := agentPid.Load()
			if !escalated.Swap(true) && pid != 0 {
				_ = syscall.Kill(int(pid), syscall.SIGTERM)
				continue
			}
			_ = cmd.Process.Kill()
		}
	}()

	// bwrap's monitor exits as soon as the agent does — the sandbox's pid 1
	// reports the agent's status the moment it is reaped, stray sandbox
	// processes notwithstanding — and carries the agent's exit status
	// (128+signal if the agent died to a signal).
	waitErr := cmd.Wait()
	werr := <-writeErr

	// A failed args write means bwrap never received the full option list —
	// whatever it did afterward was not the sandbox flar intended — so no
	// exit status from it (not even success) may be reported as the
	// agent's. The usual cause is bwrap dying before draining the pipe
	// (EPIPE), so include its status; bwrap's own message is on stderr.
	if werr != nil {
		if waitErr != nil {
			return 0, fmt.Errorf("failed to write bwrap args: %w (bwrap: %v)", werr, waitErr)
		}
		return 0, fmt.Errorf("failed to write bwrap args: %w", werr)
	}
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		// If the monitor itself died to a signal (teardown escalation above,
		// or a group-directed kill from outside), report it shell-style.
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	}
	return 0, waitErr
}

// firstChild returns the pid of the given process's first child, polling
// /proc until one appears or the deadline passes. Reading the kernel's
// children list (/proc/<pid>/task/<pid>/children) is how flar learns the
// agent's host-visible pid from outside the sandbox. If the process exits
// first (or /proc lacks the children file), an error is returned and the
// caller degrades from graceful TERM to hard teardown.
func firstChild(pid int, deadline time.Time) (int, error) {
	path := fmt.Sprintf("/proc/%d/task/%d/children", pid, pid)
	for {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			return strconv.Atoi(fields[0])
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("process %d has no children", pid)
		}
		time.Sleep(2 * time.Millisecond)
	}
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
