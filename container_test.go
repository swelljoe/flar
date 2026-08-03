package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestEncodeBwrapArgs(t *testing.T) {
	// Each argument must be nul-terminated, including the last, which bwrap's
	// --args parser requires.
	got := encodeBwrapArgs([]string{"--bind", "/a b", "/dest"})
	want := []byte("--bind\x00/a b\x00/dest\x00")
	if !bytes.Equal(got, want) {
		t.Errorf("encodeBwrapArgs = %q, want %q", got, want)
	}

	if got := encodeBwrapArgs(nil); len(got) != 0 {
		t.Errorf("encodeBwrapArgs(nil) = %q, want empty", got)
	}
}

// TestRedactedArgs verifies that the verbose command dump never prints
// credential values: --setenv values are masked unless the variable is
// explicitly marked safe to display, and the masking is fail-closed for
// variables nobody has reviewed yet.
func TestRedactedArgs(t *testing.T) {
	args := []string{
		"--setenv", "PATH", "/usr/bin",
		"--setenv", "ANTHROPIC_API_KEY", "sk-ant-secret",
		"--setenv", "SOME_FUTURE_CREDENTIAL", "hunter2",
		"--bind", "/a", "/b",
	}
	got := strings.Join(redactedArgs(args), " ")

	for _, secret := range []string{"sk-ant-secret", "hunter2"} {
		if strings.Contains(got, secret) {
			t.Errorf("redactedArgs leaked secret value %q in: %s", secret, got)
		}
	}
	// Variable names stay visible (needed for debugging), as do values of
	// explicitly safe variables.
	for _, want := range []string{"ANTHROPIC_API_KEY <redacted>", "SOME_FUTURE_CREDENTIAL <redacted>", "PATH /usr/bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("redactedArgs output missing %q: %s", want, got)
		}
	}
}

// TestRedactedArgsCoversAgentCredentials verifies that every credential
// variable forwarded into any sandbox is redacted in verbose output — i.e.
// nobody added a credential to agentEnvVars that the verbose dump would print.
func TestRedactedArgsCoversAgentCredentials(t *testing.T) {
	for agent, creds := range agentEnvVars {
		for _, env := range creds {
			if verboseVisibleEnvVars[env] {
				t.Errorf("credential var %q (agent %q) is marked visible in verbose output", env, agent)
			}
		}
	}
}

// TestEnvVarsForAgentIncludesXDG verifies that XDG_CONFIG_HOME and
// XDG_STATE_HOME are forwarded into every sandbox. Without them, pool inside
// the sandbox resolves its config/state directories to the default ~/.config
// and ~/.local/state paths, missing the bind mounts that flar set up at the
// user's XDG locations.
func TestEnvVarsForAgentIncludesXDG(t *testing.T) {
	for agent := range agentEnvVars {
		for _, env := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
			if !slices.Contains(envVarsForAgent(agent), env) {
				t.Errorf("envVarsForAgent(%q) is missing %q; pool would not find its bind-mounted config/state inside the sandbox", agent, env)
			}
		}
	}
}

// TestEnvVarsForAgentIncludesPoolAPIKey verifies that Pool's auth env vars
// are forwarded so it can authenticate inside the sandbox.
func TestEnvVarsForAgentIncludesPoolAPIKey(t *testing.T) {
	for _, env := range []string{"POOLSIDE_API_KEY", "POOLSIDE_API_URL"} {
		if !slices.Contains(envVarsForAgent(AgentPool), env) {
			t.Errorf("envVarsForAgent(AgentPool) is missing %q", env)
		}
	}
}

// TestEnvVarsForAgentScopesCredentials verifies that each agent receives only
// its own credential environment variables. Forwarding every agent's API keys
// into every sandbox would let a compromised or prompt-injected agent read
// unrelated secrets — exactly the blast area flar is meant to contain.
func TestEnvVarsForAgentScopesCredentials(t *testing.T) {
	for agent, creds := range agentEnvVars {
		vars := envVarsForAgent(agent)
		// The agent's own credentials must be present.
		for _, env := range creds {
			if !slices.Contains(vars, env) {
				t.Errorf("envVarsForAgent(%q) is missing its own credential var %q", agent, env)
			}
		}
		// Every OTHER agent's credentials must be absent.
		for other, otherCreds := range agentEnvVars {
			if other == agent {
				continue
			}
			for _, env := range otherCreds {
				if slices.Contains(vars, env) {
					t.Errorf("envVarsForAgent(%q) leaks %q, which belongs to agent %q", agent, env, other)
				}
			}
		}
	}
}

// TestSandboxAgentScriptExecsAgent verifies the script run inside the
// sandbox ends by exec'ing the agent in the foreground, with the helper
// daemons backgrounded before it. The exec is what keeps the agent fully
// interactive: it inherits flar's stdin, controlling terminal and process
// group. Nothing cleverer is needed for lifecycle either — bwrap's monitor
// exits with the agent's status the moment the agent does, even while
// orphans (podman's pause process, the proxies) are still alive in the
// sandbox, and --die-with-parent then collapses the namespace.
func TestSandboxAgentScriptExecsAgent(t *testing.T) {
	opts := RunOpts{Network: "isolated", AllowPorts: []int{8080}}
	script := sandboxAgentScript(opts, "/usr/local/bin/flar", "/home/u/.agy-secret")

	if !strings.HasSuffix(strings.TrimSpace(script), "exec \"$@\"") {
		t.Errorf("script must end by exec'ing the agent in the foreground:\n%s", script)
	}
	for _, daemon := range []string{
		"/usr/local/bin/flar --internal-proxy 9090 /run/flar-net/http-proxy.sock &",
		"/usr/local/bin/flar --internal-proxy 8080 /run/flar-net/port-8080.sock &",
		"/usr/local/bin/flar --internal-secretsvc",
	} {
		if !strings.Contains(script, daemon) {
			t.Errorf("script missing helper daemon %q:\n%s", daemon, script)
		}
	}
	// Process-lifecycle tricks in this script have broken interactivity
	// before: backgrounding the agent nulled its stdin (non-interactive
	// shells redirect async jobs' stdin to /dev/null), setsid dropped the
	// controlling terminal every TUI needs, and status/control fd juggling
	// existed only to learn what bwrap already reports. Keep the launch a
	// plain exec.
	for _, banned := range []string{"setsid", "0<&0", ">&4", "<&5", "4>&-", "5>&-", "trap", "agent_pid"} {
		if strings.Contains(script, banned) {
			t.Errorf("script contains %q; the agent launch must stay a plain exec:\n%s", banned, script)
		}
	}
}

// TestFirstChild verifies the /proc children lookup RunSandbox uses to find
// the agent's host-visible pid (bwrap monitor → sandbox pid 1 → agent).
func TestFirstChild(t *testing.T) {
	// The trailing ":" keeps bash from exec-replacing itself with sleep, so
	// the tree is bash → sleep and firstChild(bash) must find the sleep.
	cmd := exec.Command("/bin/bash", "-c", "sleep 30; :")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	pid, err := firstChild(cmd.Process.Pid, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("firstChild: %v", err)
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil || strings.TrimSpace(string(comm)) != "sleep" {
		t.Errorf("firstChild found pid %d (comm %q, err %v), want the sleep child", pid, strings.TrimSpace(string(comm)), err)
	}

	// A childless process reports an error once the deadline passes; the
	// caller then degrades from graceful TERM to hard teardown.
	if _, err := firstChild(pid, time.Now().Add(50*time.Millisecond)); err == nil {
		t.Errorf("firstChild on a childless process must fail after the deadline")
	}

	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// TestTiocstiSeccompFilter verifies the BPF program flar feeds to bwrap
// --seccomp: it must be a well-formed filter that denies ioctl(TIOCSTI) —
// the terminal keystroke-injection vector the interactive sandbox would
// otherwise expose — and nothing else. The runtime half loads the filter
// into the test process (irreversible, but it only denies TIOCSTI, which
// nothing else in the binary uses) and probes with a non-tty fd, where the
// filter firing is distinguishable from the kernel's own fd rejection
// (EPERM vs ENOTTY).
func TestTiocstiSeccompFilter(t *testing.T) {
	f := tiocstiSeccompFilter()
	if len(f)%8 != 0 {
		t.Fatalf("filter length %d is not a multiple of struct sock_filter (8 bytes)", len(f))
	}
	insn := func(i int) (code uint16, jt, jf uint8, k uint32) {
		off := i * 8
		code = binary.LittleEndian.Uint16(f[off : off+2])
		jt, jf = f[off+2], f[off+3]
		k = binary.LittleEndian.Uint32(f[off+4 : off+8])
		return
	}
	// Starts by loading the architecture word from seccomp_data.
	if code, _, _, _ := insn(0); code != 0x20 || binary.LittleEndian.Uint32(f[4:8]) != 4 {
		t.Fatalf("first instruction is not ld [4] (arch): % x", f[:8])
	}
	// A JEQ against TIOCSTI (0x5412) must be present.
	foundTiocsti := false
	for i := 0; i < len(f)/8; i++ {
		if code, _, _, k := insn(i); code == 0x15 && k == 0x5412 {
			foundTiocsti = true
		}
	}
	if !foundTiocsti {
		t.Fatalf("filter never tests for TIOCSTI (0x5412): % x", f)
	}
	// Ends with the two RETs: EPERM for the denied ioctl, then ALLOW.
	last := len(f)/8 - 1
	if code, _, _, k := insn(last); code != 0x06 || k != 0x7fff0000 {
		t.Fatalf("final instruction is not SECCOMP_RET_ALLOW: % x", f)
	}
	if code, _, _, k := insn(last - 1); code != 0x06 || k&0xffff != uint32(syscall.EPERM) {
		t.Fatalf("second-to-last instruction is not SECCOMP_RET_ERRNO|EPERM: % x", f)
	}

	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		t.Skipf("filter only denies TIOCSTI on amd64/arm64; running on %s", runtime.GOARCH)
	}

	// Load the filter into this process and prove it fires.
	filter := make([]byte, len(f))
	copy(filter, f)
	prog := struct {
		length uint16
		_      uint16
		filter uintptr
	}{length: uint16(len(filter) / 8), filter: uintptr(unsafe.Pointer(&filter[0]))}
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, 38, 1, 0); errno != 0 { // PR_SET_NO_NEW_PRIVS
		t.Skipf("cannot set no_new_privs: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, 22, 2, uintptr(unsafe.Pointer(&prog))); errno != 0 { // PR_SET_SECCOMP, SECCOMP_MODE_FILTER
		t.Skipf("kernel refused the filter: %v", errno)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	// On a non-tty fd the kernel would reject any ioctl with ENOTTY; EPERM
	// here proves the seccomp filter intercepted TIOCSTI first.
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, r.Fd(), 0x5412, 0); errno != syscall.EPERM {
		t.Fatalf("ioctl(TIOCSTI) after loading filter: errno %v, want EPERM (filter did not fire)", errno)
	}
	// A neighboring ioctl must NOT be denied: it should fall through to the
	// kernel's own ENOTTY for a pipe fd, proving the filter is narrow.
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, r.Fd(), 0x5413, 0); errno != syscall.ENOTTY {
		t.Fatalf("ioctl(0x5413) after loading filter: errno %v, want ENOTTY (filter over-broad?)", errno)
	}
}

// containsSequence reports whether seq appears contiguously in args.
func containsSequence(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

// TestContainerCacheDir verifies the cache location convention: under
// ~/.cache/flar/containers by default (never podman's own storage location,
// so flar's cache can't collide with host-side podman use), and honoring an
// absolute XDG_CACHE_HOME.
func TestContainerCacheDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	if got, want := containerCacheDir("/home/u"), "/home/u/.cache/flar/containers"; got != want {
		t.Errorf("containerCacheDir default = %q, want %q", got, want)
	}

	t.Setenv("XDG_CACHE_HOME", "/custom/cache")
	if got, want := containerCacheDir("/home/u"), "/custom/cache/flar/containers"; got != want {
		t.Errorf("containerCacheDir with XDG_CACHE_HOME = %q, want %q", got, want)
	}

	// A relative XDG_CACHE_HOME is ignored, matching the pool/mimo helpers.
	t.Setenv("XDG_CACHE_HOME", "relative/cache")
	if got, want := containerCacheDir("/home/u"), "/home/u/.cache/flar/containers"; got != want {
		t.Errorf("containerCacheDir with relative XDG_CACHE_HOME = %q, want %q", got, want)
	}
}

// TestContainerSupportArgsEphemeral verifies the sandbox setup for podman
// without caching and without a host /etc/containers: storage lives in the
// sandbox's ephemeral home (no host bind), and all files podman 5 requires
// are generated and mounted.
func TestContainerSupportArgsEphemeral(t *testing.T) {
	tempConfig := t.TempDir()
	home := t.TempDir()

	args, err := containerSupportArgs(tempConfig, home, 1000, false, "")
	if err != nil {
		t.Fatalf("containerSupportArgs: %v", err)
	}

	// No cache bind: nothing under the host home is mounted read-write
	// (read-only config files like storage.conf are fine).
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && strings.HasPrefix(args[i+2], home) {
			t.Errorf("ephemeral mode bind-mounts %q read-write into the sandbox; storage must stay on the sandbox tmpfs", args[i+2])
		}
	}

	etcContainers := filepath.Join(tempConfig, "podman", "etc-containers")
	if !containsSequence(args, "--ro-bind", etcContainers, "/etc/containers") {
		t.Errorf("etc-containers dir not mounted at /etc/containers: %v", args)
	}

	// storage.conf must sit at the user-level path: rootless podman ignores
	// graphroot/runroot from system-wide configs.
	storageConf := filepath.Join(tempConfig, "podman", "storage.conf")
	homeCfgConf := filepath.Join(home, ".config", "containers", "storage.conf")
	if !containsSequence(args, "--ro-bind", storageConf, homeCfgConf) {
		t.Errorf("storage.conf not mounted at %s: %v", homeCfgConf, args)
	}
	content, err := os.ReadFile(storageConf)
	if err != nil {
		t.Fatalf("storage.conf not written: %v", err)
	}
	// podman's build-commit path stages under /var/tmp even when TMPDIR is
	// set, so the sandbox must provide it.
	if !containsSequence(args, "--dir", "/var/tmp") {
		t.Errorf("/var/tmp not created in the sandbox: %v", args)
	}
	ephemeralGraphroot := filepath.Join(home, ".local", "share", "containers", "storage")
	for _, want := range []string{
		`graphroot = "` + ephemeralGraphroot + `"`,
		`runroot = "/run/user/1000/containers"`,
		`ignore_chown_errors = "true"`,
	} {
		if !strings.Contains(string(content), want) {
			t.Errorf("storage.conf missing %q:\n%s", want, content)
		}
	}

	// When the host ships no policy, flar must NOT generate one: the only
	// generatable policy is insecureAcceptAnything, which would silently
	// disable image signature verification. Image pulls fail closed instead,
	// with a warning from RunSandbox.
	if fileExists(filepath.Join(etcContainers, "policy.json")) {
		t.Errorf("flar generated a policy.json; it must refuse to author an accept-anything signature policy and let pulls fail closed")
	}

	// Empty subuid/subgid force podman's single-mapping path.
	for _, name := range []string{"subuid", "subgid"} {
		p := filepath.Join(tempConfig, "podman", name)
		if !containsSequence(args, "--ro-bind", p, "/etc/"+name) {
			t.Errorf("empty /etc/%s not mounted: %v", name, args)
		}
		if b, err := os.ReadFile(p); err != nil || len(b) != 0 {
			t.Errorf("/etc/%s source must exist and be empty: %q, %v", name, b, err)
		}
	}
}

// TestContainerSupportArgsCache verifies that caching bind-mounts flar's own
// cache dir read-write, points graphroot at it, and inherits a host
// /etc/containers (trust policy + registries) instead of generating a policy
// — while still replacing the host's rootful storage.conf.
func TestContainerSupportArgsCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	tempConfig := t.TempDir()
	home := t.TempDir()
	cacheDir := containerCacheDir(home)

	// A fake host /etc/containers with its own policy and storage.conf.
	hostEtc := filepath.Join(t.TempDir(), "etc-containers")
	if err := os.MkdirAll(hostEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostEtc, "policy.json"), []byte(`{"default":[{"type":"reject"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostEtc, "storage.conf"), []byte("[storage]\ndriver = \"vfs\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostEtc, "registries.conf"), []byte("unqualified-search-registries = [\"docker.io\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	args, err := containerSupportArgs(tempConfig, home, 1000, true, hostEtc)
	if err != nil {
		t.Fatalf("containerSupportArgs: %v", err)
	}

	if !containsSequence(args, "--bind", cacheDir, cacheDir) {
		t.Errorf("cache dir %s not bind-mounted read-write: %v", cacheDir, args)
	}
	if fi, err := os.Stat(cacheDir); err != nil || !fi.IsDir() {
		t.Errorf("cache dir was not created: %v", err)
	}

	etcContainers := filepath.Join(tempConfig, "podman", "etc-containers")
	if !containsSequence(args, "--ro-bind", etcContainers, "/etc/containers") {
		t.Errorf("merged etc-containers not mounted at /etc/containers: %v", args)
	}

	// The host's policy and registries are inherited verbatim...
	policy, err := os.ReadFile(filepath.Join(etcContainers, "policy.json"))
	if err != nil || !strings.Contains(string(policy), "reject") {
		t.Errorf("host policy.json not inherited: %q, %v", policy, err)
	}
	if !fileExists(filepath.Join(etcContainers, "registries.conf")) {
		t.Errorf("host registries.conf not copied into the mounted /etc/containers")
	}
	// ...and the storage.conf podman actually honors (the user-level one) is
	// flar's, not the host's rootful config.
	storageConf := filepath.Join(tempConfig, "podman", "storage.conf")
	homeCfgConf := filepath.Join(home, ".config", "containers", "storage.conf")
	if !containsSequence(args, "--ro-bind", storageConf, homeCfgConf) {
		t.Errorf("storage.conf not mounted at %s: %v", homeCfgConf, args)
	}
	content, err := os.ReadFile(storageConf)
	if err != nil {
		t.Fatalf("storage.conf not written: %v", err)
	}
	if !strings.Contains(string(content), `graphroot = "`+cacheDir+`"`) ||
		!strings.Contains(string(content), `ignore_chown_errors = "true"`) {
		t.Errorf("storage.conf is not flar's single-mapping config:\n%s", content)
	}
	if strings.Contains(string(content), "vfs") {
		t.Errorf("host storage.conf leaked into flar's:\n%s", content)
	}
}
