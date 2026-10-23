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

func TestCopyDirPreservesSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// 1. Regular file
	origFile := filepath.Join(src, "real.txt")
	if err := os.WriteFile(origFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Relative file symlink
	relLink := filepath.Join(src, "link-rel.txt")
	if err := os.Symlink("real.txt", relLink); err != nil {
		t.Fatal(err)
	}

	// 3. Absolute file symlink
	absLink := filepath.Join(src, "link-abs.txt")
	if err := os.Symlink(origFile, absLink); err != nil {
		t.Fatal(err)
	}

	// 4. Directory symlink (pointing to external skills directory)
	externalDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(externalDir, "SKILL.md"), []byte("# Skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(src, "my-skill")
	if err := os.Symlink(externalDir, dirLink); err != nil {
		t.Fatal(err)
	}

	copied := filepath.Join(dst, "copied")
	if err := CopyDir(src, copied); err != nil {
		t.Fatalf("CopyDir failed: %v", err)
	}

	// Verify relative link
	target, err := os.Readlink(filepath.Join(copied, "link-rel.txt"))
	if err != nil {
		t.Fatalf("failed to readlink rel: %v", err)
	}
	if target != "real.txt" {
		t.Errorf("rel link target = %q, want %q", target, "real.txt")
	}

	// Verify absolute link
	target, err = os.Readlink(filepath.Join(copied, "link-abs.txt"))
	if err != nil {
		t.Fatalf("failed to readlink abs: %v", err)
	}
	if target != origFile {
		t.Errorf("abs link target = %q, want %q", target, origFile)
	}

	// Verify directory symlink
	target, err = os.Readlink(filepath.Join(copied, "my-skill"))
	if err != nil {
		t.Fatalf("failed to readlink dir link: %v", err)
	}
	if target != externalDir {
		t.Errorf("dir link target = %q, want %q", target, externalDir)
	}
}

func TestCopyDirExceptPreservesSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	skillsDir := filepath.Join(src, "config", "skills")
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	externalSkill := "/opt/skills/cool-skill"
	if err := os.Symlink(externalSkill, filepath.Join(skillsDir, "cool-skill")); err != nil {
		t.Fatal(err)
	}

	copied := filepath.Join(dst, "copied")
	if err := CopyDirExcept(src, copied, map[string]bool{}); err != nil {
		t.Fatalf("CopyDirExcept failed: %v", err)
	}

	target, err := os.Readlink(filepath.Join(copied, "config", "skills", "cool-skill"))
	if err != nil {
		t.Fatalf("failed to readlink: %v", err)
	}
	if target != externalSkill {
		t.Errorf("symlink target = %q, want %q", target, externalSkill)
	}
}

func TestCopyClaudeConfigPreservesSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	externalSkills := t.TempDir()
	if err := os.Symlink(externalSkills, filepath.Join(src, "skills")); err != nil {
		t.Fatal(err)
	}

	if err := copyClaudeConfig(src, dst, "/workspace"); err != nil {
		t.Fatalf("copyClaudeConfig failed: %v", err)
	}

	target, err := os.Readlink(filepath.Join(dst, "skills"))
	if err != nil {
		t.Fatalf("failed to readlink: %v", err)
	}
	if target != externalSkills {
		t.Errorf("claude skills symlink target = %q, want %q", target, externalSkills)
	}
}
