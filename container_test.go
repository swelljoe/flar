package main

import (
	"bytes"
	"slices"
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
