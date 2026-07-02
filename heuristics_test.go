package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string // filename -> content
		expected Language
	}{
		{
			name:     "Go project with go.mod",
			files:    map[string]string{"go.mod": "module test"},
			expected: LangGo,
		},
		{
			name:     "Rust project with Cargo.toml",
			files:    map[string]string{"Cargo.toml": ""},
			expected: LangRust,
		},
		{
			name:     "Python project with pyproject.toml",
			files:    map[string]string{"pyproject.toml": ""},
			expected: LangPython,
		},
		{
			name:     "TypeScript project with package.json",
			files:    map[string]string{"package.json": ""},
			expected: LangTypeScript,
		},
		{
			name:     "Perl project with Makefile.PL",
			files:    map[string]string{"Makefile.PL": ""},
			expected: LangPerl,
		},
		{
			name:     "Python from gitignore",
			files:    map[string]string{".gitignore": "# python\n__pycache__/\n.venv/"},
			expected: LangPython,
		},
		{
			name:     "Rust from gitignore",
			files:    map[string]string{".gitignore": "target/\n"},
			expected: LangRust,
		},
		{
			name:     "TypeScript from gitignore",
			files:    map[string]string{".gitignore": "node_modules/\n"},
			expected: LangTypeScript,
		},
		{
			name:     "Perl from gitignore",
			files:    map[string]string{".gitignore": "blib/\n"},
			expected: LangPerl,
		},
		{
			name:     "Generic project with random files",
			files:    map[string]string{"README.md": "", "data.csv": ""},
			expected: LangGeneric,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a temp directory for the test
			tempDir, err := os.MkdirTemp("", "flar-test-*")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tempDir)

			// Write test files
			for filename, content := range tt.files {
				filePath := filepath.Join(tempDir, filename)
				if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
					t.Fatalf("failed to write test file %s: %v", filename, err)
				}
			}

			// Run detection
			got := DetectLanguage(tempDir)
			if got != tt.expected {
				t.Errorf("DetectLanguage() = %v, want %v", got, tt.expected)
			}
		})
	}
}
