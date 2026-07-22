package main

import (
	"bytes"
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
