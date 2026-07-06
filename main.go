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
	Agent      string `json:"agent"`
	Ask        bool   `json:"ask"`
	AllowPorts []int  `json:"allow_ports"`
	Network    string `json:"network"`
}

type intSlice []int

func (i *intSlice) String() string {
	return fmt.Sprintf("%v", *i)
}

func (i *intSlice) Set(value string) error {
	var val int
	if _, err := fmt.Sscan(value, &val); err != nil {
		return err
	}
	*i = append(*i, val)
	return nil
}

func main() {
	// Internal Secret Service execution inside the sandbox. The secret is passed
	// via env (FLAR_AGY_SECRET), not argv, to keep it off the process list.
	if len(os.Args) >= 3 && os.Args[1] == "--internal-secretsvc" {
		socketPath := os.Args[2]
		secret := os.Getenv("FLAR_AGY_SECRET")
		if f := os.Getenv("FLAR_AGY_SECRET_FILE"); f != "" {
			if b, err := os.ReadFile(f); err == nil {
				secret = string(b)
			}
		}
		if err := RunSecretService(socketPath, secret); err != nil {
			fmt.Fprintf(os.Stderr, "secretsvc error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Check if this is an internal proxy execution inside the sandbox
	if len(os.Args) >= 4 && os.Args[1] == "--internal-proxy" {
		portStr := os.Args[2]
		socketPath := os.Args[3]
		var port int
		if _, err := fmt.Sscan(portStr, &port); err != nil {
			fmt.Fprintf(os.Stderr, "Invalid port: %v\n", err)
			os.Exit(1)
		}
		RunSandboxProxy(port, socketPath)
		os.Exit(0)
	}

	// 1. Define command-line flags
	agentFlag := flag.String("m", "", "Specify the agent to run (claude, codex, agy, copilot, reasonix)")
	askFlag := flag.Bool("ask", false, "Disable bypass of agent permissions/approvals (ask for permission)")
	networkFlag := flag.String("network", "", "Network mode: isolated (default) or host")
	verboseFlag := flag.Bool("v", false, "Enable verbose logging")

	var allowPortsFlag intSlice
	flag.Var(&allowPortsFlag, "allow-port", "Allow a specific local TCP port through the network sandbox (can specify multiple)")

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
	case AgentClaude, AgentCodex, AgentAgy, AgentCopilot, AgentReasonix:
		// Valid
	default:
		fmt.Fprintf(os.Stderr, "Error: Unknown or unsupported agent: %s\n", selectedAgent)
		os.Exit(1)
	}

	askMode := *askFlag
	if !askMode && config.Ask {
		askMode = true
	}

	networkMode := *networkFlag
	if networkMode == "" {
		if config.Network != "" {
			networkMode = config.Network
		} else {
			networkMode = "isolated"
		}
	}

	if networkMode != "isolated" && networkMode != "host" {
		fmt.Fprintf(os.Stderr, "Error: Unknown network mode: %s (must be 'isolated' or 'host')\n", networkMode)
		os.Exit(1)
	}

	// Merge allowed ports from config and CLI flags
	var allowPorts []int
	portMap := make(map[int]bool)
	for _, p := range config.AllowPorts {
		if !portMap[p] {
			portMap[p] = true
			allowPorts = append(allowPorts, p)
		}
	}
	for _, p := range allowPortsFlag {
		if !portMap[p] {
			portMap[p] = true
			allowPorts = append(allowPorts, p)
		}
	}

	if *verboseFlag {
		fmt.Printf("Workspace: %s\n", absProjectDir)
		fmt.Printf("Detected/Selected Agent: %s\n", selectedAgent)
		fmt.Printf("Ask Mode: %v\n", askMode)
		fmt.Printf("Network Mode: %s\n", networkMode)
		if len(allowPorts) > 0 {
			fmt.Printf("Allowed Local Ports: %v\n", allowPorts)
		}
	}

	// 4. Copy credentials/configs to temp directory for mapping
	tempConfig, err := PrepareConfigDir(selectedAgent, absProjectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to prepare credentials: %v. Running anyway...\n", err)
	}
	if tempConfig != "" {
		defer os.RemoveAll(tempConfig)
	}

	// 5. Setup host-side network proxies if in isolated network mode
	var tempNetDir string
	var proxies []*PortProxy
	var httpProxy *HttpProxy

	if networkMode == "isolated" {
		tempNetDir, err = os.MkdirTemp("", "flar-net-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating network proxy directory: %v\n", err)
			os.Exit(1)
		}
		defer os.RemoveAll(tempNetDir)

		// Start HTTP/HTTPS Proxy on host
		httpProxySock := filepath.Join(tempNetDir, "http-proxy.sock")
		httpProxy, err = StartHttpProxy(httpProxySock, allowPorts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error starting host HTTP proxy: %v\n", err)
			os.Exit(1)
		}
		defer httpProxy.Close()

		// Start TCP Port Proxies on host
		for _, port := range allowPorts {
			sockPath := filepath.Join(tempNetDir, fmt.Sprintf("port-%d.sock", port))
			proxy, err := StartHostProxy(port, sockPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error starting host TCP proxy for port %d: %v\n", port, err)
				for _, p := range proxies {
					p.Close()
				}
				os.Exit(1)
			}
			proxies = append(proxies, proxy)
		}
	}
	defer func() {
		for _, p := range proxies {
			p.Close()
		}
	}()

	// 6. Run the Bubblewrap sandbox
	err = RunSandbox(RunOpts{
		ProjectDir: absProjectDir,
		TempConfig: tempConfig,
		TempNetDir: tempNetDir,
		AllowPorts: allowPorts,
		Agent:      selectedAgent,
		Network:    networkMode,
		AskMode:    askMode,
		Verbose:    *verboseFlag,
		ExtraArgs:  agentArgs,
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running sandbox: %v\n", err)
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

		// Check 5. Reasonix
		if fileExists(filepath.Join(home, ".reasonix")) {
			return AgentReasonix
		}
		if _, exists := os.LookupEnv("DEEPSEEK_API_KEY"); exists {
			return AgentReasonix
		}
	}

	// Default fallback
	return AgentClaude
}
