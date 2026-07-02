package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Agent represents the supported AI developer agents.
type Agent string

const (
	AgentClaude  Agent = "claude"
	AgentCodex   Agent = "codex"
	AgentAgy     Agent = "agy"
	AgentCopilot Agent = "copilot"
)

// Default Containerfile templates
const baseFedoraImage = "registry.fedoraproject.org/fedora:44"

// GetDefaultContainerfile returns the built-in Containerfile content for the given language and agent.
func GetDefaultContainerfile(lang Language, agent Agent) string {
	var packages string
	switch lang {
	case LangGo:
		packages = "golang gcc make git ripgrep fd-find findutils coreutils tar unzip procps-ng"
	case LangRust:
		packages = "rust cargo gcc make git ripgrep fd-find findutils coreutils tar unzip procps-ng"
	case LangPython:
		packages = "python3 python3-pip python3-uv gcc make git ripgrep fd-find findutils coreutils tar unzip procps-ng"
	case LangTypeScript:
		packages = "nodejs npm gcc make git ripgrep fd-find findutils coreutils tar unzip procps-ng"
	case LangPerl:
		packages = "perl make gcc git ripgrep fd-find findutils coreutils tar unzip procps-ng"
	default:
		packages = "git ripgrep fd-find findutils coreutils tar unzip procps-ng"
	}

	content := fmt.Sprintf("FROM %s\n", baseFedoraImage)
	content += fmt.Sprintf("RUN dnf install -y %s && dnf clean all\n", packages)

	// Install Node.js/npm if the agent requires it and the language template doesn't already have it.
	agentRequiresNode := (agent == AgentClaude || agent == AgentCodex || agent == AgentCopilot)
	if agentRequiresNode && lang != LangTypeScript {
		content += "RUN dnf install -y nodejs npm && dnf clean all\n"
	}

	// Agent-specific installation steps
	switch agent {
	case AgentClaude:
		content += "RUN npm install -g @anthropic-ai/claude-code\n"
	case AgentCodex:
		content += "RUN npm install -g @openai/codex\n"
	case AgentCopilot:
		content += "RUN dnf install -y github-cli && dnf clean all && npm install -g @githubnext/github-copilot-cli\n"
	case AgentAgy:
		// Agy is copied/mounted at runtime, no container build step needed.
	}

	content += "WORKDIR /workspace\n"
	return content
}

// ResolveContainerfile looks for custom Containerfiles on the disk.
// Returns: (customPath, inlineContent, error)
func ResolveContainerfile(projectDir string, lang Language, agent Agent) (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}

	// List of potential paths to search for user-defined Containerfiles
	searchPaths := []string{
		filepath.Join(projectDir, ".flar", "Containerfile"),
		filepath.Join(projectDir, ".flar", fmt.Sprintf("%s.%s.Containerfile", lang, agent)),
		filepath.Join(projectDir, ".flar", fmt.Sprintf("%s.Containerfile", lang)),
		filepath.Join(home, ".config", "flar", "templates", fmt.Sprintf("%s.%s.Containerfile", lang, agent)),
		filepath.Join(home, ".config", "flar", "templates", fmt.Sprintf("%s.Containerfile", lang)),
		filepath.Join(home, ".config", "flar", "templates", fmt.Sprintf("generic.%s.Containerfile", agent)),
		filepath.Join(home, ".config", "flar", "templates", "generic.Containerfile"),
	}

	for _, path := range searchPaths {
		if _, err := os.Stat(path); err == nil {
			return path, "", nil
		}
	}

	// Fallback to default built-in templates
	return "", GetDefaultContainerfile(lang, agent), nil
}
