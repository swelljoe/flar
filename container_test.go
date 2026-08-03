package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

	// Without a host policy, a permissive policy.json must be generated.
	policy, err := os.ReadFile(filepath.Join(etcContainers, "policy.json"))
	if err != nil || !strings.Contains(string(policy), "insecureAcceptAnything") {
		t.Errorf("policy.json missing or invalid: %q, %v", policy, err)
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
