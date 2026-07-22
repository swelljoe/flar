package main

import (
	"os"
	"path/filepath"
	"testing"
)

// unsetAgentEnvs clears all the environment variables that autoDetectAgent
// consults for API-key based detection, so tests can isolate file-based
// detection. The variables are truly unset (not set to empty), because
// autoDetectAgent uses os.LookupEnv which reports existence even for empty
// values.
func unsetAgentEnvs(t *testing.T) {
	t.Helper()
	for _, env := range []string{
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"GEMINI_API_KEY",
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"COPILOT_GITHUB_TOKEN",
		"DEEPSEEK_API_KEY",
		"KIMI_API_KEY",
		"POOLSIDE_API_KEY",
	} {
		old, hadOld := os.LookupEnv(env)
		if err := os.Unsetenv(env); err != nil {
			t.Fatalf("os.Unsetenv(%q): %v", env, err)
		}
		t.Cleanup(func() {
			if hadOld {
				os.Setenv(env, old)
			}
		})
	}
}

// TestAutoDetectAgentPoolXDGConfigHome verifies that Pool is auto-detected
// when its config directory lives under a non-default XDG_CONFIG_HOME. This
// is the regression test for the reviewer's comment that autoDetectAgent
// previously hardcoded ~/.config/poolside.
func TestAutoDetectAgentPoolXDGConfigHome(t *testing.T) {
	unsetAgentEnvs(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	// Place the pool config under a custom XDG_CONFIG_HOME.
	xdgConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfig)
	if err := os.MkdirAll(filepath.Join(xdgConfig, "poolside"), 0o700); err != nil {
		t.Fatal(err)
	}

	if got := autoDetectAgent(); got != AgentPool {
		t.Errorf("autoDetectAgent() = %q, want %q (pool config under XDG_CONFIG_HOME=%s)",
			got, AgentPool, xdgConfig)
	}
}

// TestAutoDetectAgentPoolDefaultConfigHome verifies that Pool is still
// auto-detected via the default ~/.config/poolside location when
// XDG_CONFIG_HOME is unset.
func TestAutoDetectAgentPoolDefaultConfigHome(t *testing.T) {
	unsetAgentEnvs(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	if err := os.MkdirAll(filepath.Join(home, ".config", "poolside"), 0o700); err != nil {
		t.Fatal(err)
	}

	if got := autoDetectAgent(); got != AgentPool {
		t.Errorf("autoDetectAgent() = %q, want %q (pool config under default ~/.config/poolside)",
			got, AgentPool)
	}
}

// TestAutoDetectAgentPoolAPIKey verifies that Pool is auto-detected via the
// POOLSIDE_API_KEY environment variable.
func TestAutoDetectAgentPoolAPIKey(t *testing.T) {
	unsetAgentEnvs(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("POOLSIDE_API_KEY", "test-key")

	if got := autoDetectAgent(); got != AgentPool {
		t.Errorf("autoDetectAgent() = %q, want %q (POOLSIDE_API_KEY set)",
			got, AgentPool)
	}
}

// TestAutoDetectAgentPoolNoConfig verifies that Pool is NOT auto-detected when
// no pool config exists and no POOLSIDE_API_KEY is set, falling back to the
// default (Claude).
func TestAutoDetectAgentPoolNoConfig(t *testing.T) {
	unsetAgentEnvs(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	if got := autoDetectAgent(); got != AgentClaude {
		t.Errorf("autoDetectAgent() = %q, want %q (no pool config present)",
			got, AgentClaude)
	}
}
