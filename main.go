package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Config represents the settings read from a config file.
type Config struct {
	Agent    string `json:"agent"`
	Template string `json:"template"`
	Ask      bool   `json:"ask"`
}

func main() {
	// 1. Define command-line flags
	agentFlag := flag.String("m", "", "Specify the agent to run (claude, codex, agy, copilot)")
	templateFlag := flag.String("t", "", "Override the language template (go, rust, python, typescript, perl, generic)")
	askFlag := flag.Bool("ask", false, "Disable bypass of agent permissions/approvals (ask for permission)")
	rebuildFlag := flag.Bool("rebuild", false, "Force rebuilding the container image")
	verboseFlag := flag.Bool("v", false, "Enable verbose logging")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: flar [flags] [path/to/project] [extra agent args/prompts...]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	// 2. Determine target workspace directory and extra agent arguments
	projectDir := "."
	var agentArgs []string

	args := flag.Args()
	if len(args) > 0 {
		firstArg := args[0]
		info, err := os.Stat(firstArg)
		if err == nil && info.IsDir() {
			projectDir = firstArg
			agentArgs = args[1:]
		} else {
			projectDir = "."
			agentArgs = args
		}
	}

	absProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving project path: %v\n", err)
		os.Exit(1)
	}

	// 3. Load configurations from config file if available
	config := loadConfig(absProjectDir)

	// Command line flags override configuration file settings
	selectedAgent := Agent(*agentFlag)
	if selectedAgent == "" {
		if config.Agent != "" {
			selectedAgent = Agent(config.Agent)
		} else {
			selectedAgent = autoDetectAgent()
		}
	}

	// Validate agent
	switch selectedAgent {
	case AgentClaude, AgentCodex, AgentAgy, AgentCopilot:
		// Valid
	default:
		fmt.Fprintf(os.Stderr, "Error: Unknown or unsupported agent: %s\n", selectedAgent)
		os.Exit(1)
	}

	selectedLang := Language(*templateFlag)
	if selectedLang == "" {
		if config.Template != "" {
			selectedLang = Language(config.Template)
		} else {
			selectedLang = DetectLanguage(absProjectDir)
		}
	}

	// Validate template language
	switch selectedLang {
	case LangGo, LangRust, LangPython, LangTypeScript, LangPerl, LangGeneric:
		// Valid
	default:
		fmt.Fprintf(os.Stderr, "Error: Unknown or unsupported language template: %s\n", selectedLang)
		os.Exit(1)
	}

	askMode := *askFlag
	if !askMode && config.Ask {
		askMode = true
	}

	if *verboseFlag {
		fmt.Printf("Workspace: %s\n", absProjectDir)
		fmt.Printf("Detected/Selected Language Template: %s\n", selectedLang)
		fmt.Printf("Detected/Selected Agent: %s\n", selectedAgent)
		fmt.Printf("Ask Mode: %v\n", askMode)
	}

	// 4. Resolve the Containerfile (custom or default)
	customPath, inlineContent, err := ResolveContainerfile(absProjectDir, selectedLang, selectedAgent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving Containerfile: %v\n", err)
		os.Exit(1)
	}

	// 5. Check and build the container image
	imageName := fmt.Sprintf("flar-%s-%s:latest", selectedLang, selectedAgent)
	exists, err := ImageExists(imageName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking Podman image: %v\n", err)
		os.Exit(1)
	}

	if !exists || *rebuildFlag {
		if *verboseFlag {
			fmt.Printf("Image %s does not exist or rebuild requested. Building...\n", imageName)
		}
		err = BuildImage(imageName, customPath, inlineContent, *verboseFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error building image: %v\n", err)
			os.Exit(1)
		}
	}

	// 6. Copy credentials/configs to temp directory for mapping
	tempConfig, err := PrepareConfigDir(selectedAgent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to prepare credentials: %v. Running anyway...\n", err)
	}
	if tempConfig != "" {
		defer os.RemoveAll(tempConfig)
	}

	// 7. Run the container
	err = RunContainer(RunOpts{
		ImageName:  imageName,
		ProjectDir: absProjectDir,
		TempConfig: tempConfig,
		Agent:      selectedAgent,
		AskMode:    askMode,
		Verbose:    *verboseFlag,
		ExtraArgs:  agentArgs,
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running container: %v\n", err)
		os.Exit(1)
	}
}

// loadConfig reads config files from project root or user config dir.
func loadConfig(projectDir string) Config {
	var config Config

	// 1. Check project local config: <project>/.flar.json
	localPath := filepath.Join(projectDir, ".flar.json")
	if fileExists(localPath) {
		if err := readJSON(localPath, &config); err == nil {
			return config
		}
	}

	// 2. Check global config: ~/.config/flar/config.json
	home, err := os.UserHomeDir()
	if err == nil {
		globalPath := filepath.Join(home, ".config", "flar", "config.json")
		if fileExists(globalPath) {
			_ = readJSON(globalPath, &config)
		}
	}

	return config
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readJSON(path string, val interface{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(file).Decode(val)
}

// autoDetectAgent determines which agent is configured on the host.
func autoDetectAgent() Agent {
	home, err := os.UserHomeDir()
	if err == nil {
		// Check 1. Claude
		if fileExists(filepath.Join(home, ".claude")) {
			return AgentClaude
		}
		if _, exists := os.LookupEnv("ANTHROPIC_API_KEY"); exists {
			return AgentClaude
		}

		// Check 2. Codex
		if fileExists(filepath.Join(home, ".codex")) {
			return AgentCodex
		}
		if _, exists := os.LookupEnv("OPENAI_API_KEY"); exists {
			return AgentCodex
		}

		// Check 3. Agy
		if fileExists(filepath.Join(home, ".gemini")) {
			return AgentAgy
		}
		if _, exists := os.LookupEnv("GEMINI_API_KEY"); exists {
			return AgentAgy
		}

		// Check 4. Copilot
		if fileExists(filepath.Join(home, ".copilot")) || fileExists(filepath.Join(home, ".config", "gh")) {
			return AgentCopilot
		}
		for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN", "COPILOT_GITHUB_TOKEN"} {
			if _, exists := os.LookupEnv(env); exists {
				return AgentCopilot
			}
		}
	}

	// Default fallback
	return AgentClaude
}
