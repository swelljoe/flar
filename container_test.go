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

// TestPassthroughEnvVarsIncludesXDG verifies that XDG_CONFIG_HOME and
// XDG_STATE_HOME are forwarded into the sandbox. Without them, pool inside the
// sandbox resolves its config/state directories to the default ~/.config and
// ~/.local/state paths, missing the bind mounts that flar set up at the
// user's XDG locations.
func TestPassthroughEnvVarsIncludesXDG(t *testing.T) {
	for _, env := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		if !slices.Contains(passthroughEnvVars, env) {
			t.Errorf("passthroughEnvVars is missing %q; pool would not find its bind-mounted config/state inside the sandbox", env)
		}
	}
}

// TestPassthroughEnvVarsIncludesPoolAPIKey verifies that Pool's auth env vars
// are forwarded so it can authenticate inside the sandbox.
func TestPassthroughEnvVarsIncludesPoolAPIKey(t *testing.T) {
	for _, env := range []string{"POOLSIDE_API_KEY", "POOLSIDE_API_URL"} {
		if !slices.Contains(passthroughEnvVars, env) {
			t.Errorf("passthroughEnvVars is missing %q", env)
		}
	}
}
