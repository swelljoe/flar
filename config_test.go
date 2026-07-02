package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFileAndDir(t *testing.T) {
	tempSrc, err := os.MkdirTemp("", "flar-config-test-src-*")
	if err != nil {
		t.Fatalf("failed to create temp src dir: %v", err)
	}
	defer os.RemoveAll(tempSrc)

	tempDst, err := os.MkdirTemp("", "flar-config-test-dst-*")
	if err != nil {
		t.Fatalf("failed to create temp dst dir: %v", err)
	}
	defer os.RemoveAll(tempDst)

	// Create a nested file structure
	file1Path := filepath.Join(tempSrc, "file1.txt")
	file1Content := "hello world"
	if err := os.WriteFile(file1Path, []byte(file1Content), 0600); err != nil {
		t.Fatalf("failed to write file1: %v", err)
	}

	subDir := filepath.Join(tempSrc, "subdir")
	if err := os.Mkdir(subDir, 0700); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	file2Path := filepath.Join(subDir, "file2.json")
	file2Content := `{"test": true}`
	if err := os.WriteFile(file2Path, []byte(file2Content), 0644); err != nil {
		t.Fatalf("failed to write file2: %v", err)
	}

	// Copy directory
	destPath := filepath.Join(tempDst, "copied")
	if err := CopyDir(tempSrc, destPath); err != nil {
		t.Fatalf("CopyDir failed: %v", err)
	}

	// Verify copies
	gotFile1, err := os.ReadFile(filepath.Join(destPath, "file1.txt"))
	if err != nil {
		t.Fatalf("failed to read copied file1: %v", err)
	}
	if string(gotFile1) != file1Content {
		t.Errorf("copied file1 content mismatch: got %q, want %q", gotFile1, file1Content)
	}

	gotFile2, err := os.ReadFile(filepath.Join(destPath, "subdir", "file2.json"))
	if err != nil {
		t.Fatalf("failed to read copied file2: %v", err)
	}
	if string(gotFile2) != file2Content {
		t.Errorf("copied file2 content mismatch: got %q, want %q", gotFile2, file2Content)
	}
}
