package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Language represents the detected environment template.
type Language string

const (
	LangGo         Language = "go"
	LangRust       Language = "rust"
	LangPython     Language = "python"
	LangTypeScript Language = "typescript"
	LangPerl       Language = "perl"
	LangGeneric    Language = "generic"
)

// DetectLanguage analyzes the project directory to guess the target language.
func DetectLanguage(dir string) Language {
	// 1. Check explicit project files in the base directory
	if hasFile(dir, "go.mod") || hasFile(dir, "go.work") {
		return LangGo
	}
	if hasFile(dir, "Cargo.toml") {
		return LangRust
	}
	if hasFile(dir, "pyproject.toml") || hasFile(dir, "requirements.txt") ||
		hasFile(dir, "poetry.lock") || hasFile(dir, "Pipfile") || hasFile(dir, "setup.py") {
		return LangPython
	}
	if hasFile(dir, "package.json") || hasFile(dir, "tsconfig.json") {
		return LangTypeScript
	}
	if hasFile(dir, "Makefile.PL") || hasFile(dir, "Build.PL") || hasFile(dir, "cpanfile") {
		return LangPerl
	}

	// 2. Check for file extensions in the directory
	if hasExtension(dir, ".pl") || hasExtension(dir, ".pm") {
		return LangPerl
	}
	if hasExtension(dir, ".go") {
		return LangGo
	}
	if hasExtension(dir, ".rs") {
		return LangRust
	}
	if hasExtension(dir, ".py") {
		return LangPython
	}
	if hasExtension(dir, ".ts") || hasExtension(dir, ".tsx") {
		return LangTypeScript
	}

	// 3. Look at .gitignore for patterns
	if gitignoreLang := checkGitignore(dir); gitignoreLang != LangGeneric {
		return gitignoreLang
	}

	return LangGeneric
}

func hasFile(dir, filename string) bool {
	_, err := os.Stat(filepath.Join(dir, filename))
	return err == nil
}

func hasExtension(dir, ext string) bool {
	files, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(strings.ToLower(f.Name()), ext) {
			return true
		}
	}
	return false
}

func checkGitignore(dir string) Language {
	path := filepath.Join(dir, ".gitignore")
	file, err := os.Open(path)
	if err != nil {
		return LangGeneric
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Match common patterns
		if strings.Contains(line, "node_modules/") {
			return LangTypeScript
		}
		if strings.Contains(line, "target/") || strings.Contains(line, "Cargo.lock") {
			return LangRust
		}
		if strings.Contains(line, "__pycache__") || strings.Contains(line, ".venv") ||
			strings.Contains(line, "venv/") || strings.Contains(line, "*.pyc") {
			return LangPython
		}
		if strings.Contains(line, "blib/") || strings.Contains(line, "pm_to_blib") {
			return LangPerl
		}
	}
	return LangGeneric
}
