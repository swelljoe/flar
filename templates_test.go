package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetDefaultContainerfile(t *testing.T) {
	// Test that Claude agent triggers Node installation
	content := GetDefaultContainerfile(LangGo, AgentClaude)
	if !strings.Contains(content, "dnf install -y nodejs npm") {
		t.Errorf("expected nodejs and npm to be installed for Go + Claude")
	}
	if !strings.Contains(content, "npm install -g @anthropic-ai/claude-code") {
		t.Errorf("expected claude code to be installed for Go + Claude")
	}

	// Test that Go language triggers golang installation
	if !strings.Contains(content, "golang") {
		t.Errorf("expected golang to be in packages for Go + Claude")
	}

	// Test that Agy agent does not add any extra npm install steps
	contentAgy := GetDefaultContainerfile(LangRust, AgentAgy)
	if strings.Contains(contentAgy, "npm install") {
		t.Errorf("unexpected npm installation for Rust + Agy")
	}
	if !strings.Contains(contentAgy, "rust cargo") {
		t.Errorf("expected rust and cargo to be in packages for Rust + Agy")
	}
}

func TestResolveContainerfile(t *testing.T) {
	// Create a temp directory representing project root
	projectDir, err := os.MkdirTemp("", "flar-project-*")
	if err != nil {
		t.Fatalf("failed to create temp project dir: %v", err)
	}
	defer os.RemoveAll(projectDir)

	// Resolve without custom files should return default content
	customPath, inline, err := ResolveContainerfile(projectDir, LangGo, AgentClaude)
	if err != nil {
		t.Fatalf("ResolveContainerfile failed: %v", err)
	}
	if customPath != "" {
		t.Errorf("expected customPath to be empty, got %s", customPath)
	}
	if !strings.Contains(inline, "FROM registry.fedoraproject.org/fedora:44") {
		t.Errorf("expected default inline Containerfile, got: %s", inline)
	}

	// Create a custom Containerfile in .flar/Containerfile
	flarDir := filepath.Join(projectDir, ".flar")
	if err := os.MkdirAll(flarDir, 0755); err != nil {
		t.Fatalf("failed to create .flar dir: %v", err)
	}
	customFilePath := filepath.Join(flarDir, "Containerfile")
	customContent := "FROM fedora:latest\nRUN echo custom"
	if err := os.WriteFile(customFilePath, []byte(customContent), 0644); err != nil {
		t.Fatalf("failed to write custom Containerfile: %v", err)
	}

	// Resolve again, should return customPath
	customPath, inline, err = ResolveContainerfile(projectDir, LangGo, AgentClaude)
	if err != nil {
		t.Fatalf("ResolveContainerfile failed: %v", err)
	}
	if customPath != customFilePath {
		t.Errorf("expected customPath to be %s, got %s", customFilePath, customPath)
	}
	if inline != "" {
		t.Errorf("expected inline to be empty when customPath is found, got %s", inline)
	}
}
