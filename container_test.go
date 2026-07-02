package main

import (
	"bytes"
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
