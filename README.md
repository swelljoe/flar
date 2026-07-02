# flar

FLAR is the Fast Light Agent Restrictor. It runs on rocks called gars.

It is a simple, lightweight CLI tool in Go to run coding agent CLIs (like Claude Code, Antigravity, Codex, and Copilot) safely inside isolated Bubblewrap (`bwrap`) sandboxes.

## Features

- **Bubblewrap Sandbox**: Runs the agent in an unprivileged user namespace using a clean root directory (`tmpfs`). System paths (`/usr`, `/bin`, `/lib`, `/lib64`, etc.) are mounted read-only from the host, ensuring host packages are immediately available without container image management.
- **Strict Filesystem Isolation**: Only the target project directory is bind-mounted read-write. The rest of the host home directory is hidden, protecting ssh keys, shell configurations, and personal files from prompt injection attacks.
- **Network Sandboxing**: 
  - **Isolated Mode (Default)**: The network namespace is unshared. Internet access is tunneled through a host-side HTTP/HTTPS proxy that performs DNS lookup on the host and filters out traffic to local/loopback IP addresses.
  - **Port Forwarding**: Selectively expose local services (e.g. databases, Ollama models) into the sandbox by mapping specific ports to the host's `localhost`.
  - **Host Mode**: Option to share the host's network namespace for unconstrained access.
- **Dangerous Bypass Options**: Automatically injects flags (like `--dangerously-skip-permissions` for Claude/`agy` or `--dangerously-bypass-approvals-and-sandbox` for Codex) so agents run without runtime approval interruptions. Can be disabled with `-ask`.
- **Config Copying**: Automatically copies host credentials (like `~/.claude/`, `~/.codex/`, `~/.gemini/`, or GitHub CLI configurations) to a temporary directory mounted inside the sandbox home directory, leaving host config files untouched.

## Build and Install

### Dependencies

Ensure `bwrap` (Bubblewrap) is installed on your host system:

```bash
# On Fedora/RHEL
sudo dnf install bubblewrap

# On Debian/Ubuntu
sudo apt install bubblewrap
```

### Compile & Install

To build `flar` from source:

```bash
go build -o flar .
```

To install:

```bash
mv flar ~/.local/bin/
```

## Usage

Run `flar` in your project folder or specify the path:

```bash
flar [flags] [path/to/project] [extra agent args/prompts...]
```

### Flags

- `-m`: Specify the agent to run (`claude`, `codex`, `agy`, `copilot`). Defaults to checking available host configurations or environment variables.
- `-ask`: Do not skip permissions/approvals (forcing the agent to ask for permission).
- `-network`: Network mode: `isolated` (default) or `host`.
- `-allow-port`: Allow a specific local TCP port (e.g. `8080`, `11434`) through the isolated network sandbox. Can be specified multiple times.
- `-v`: Enable verbose logging.

### Configuration file (`.flar.json`)

You can configure options per-project in `<project>/.flar.json` or globally in `~/.config/flar/config.json`:

```json
{
  "agent": "claude",
  "ask": false,
  "network": "isolated",
  "allow_ports": [5432, 11434]
}
```

## Network Security & Local Ports

In **isolated** network mode, the agent's environment has no direct access to host network interfaces.

* **Internet Access**: Works automatically for HTTP/HTTPS requests (such as connecting to cloud LLMs like Anthropic or Gemini) using the `HTTP_PROXY` and `HTTPS_PROXY` environment variables.
* **Localhost Restrictions**: Requests to `localhost` or loopback IPs via the proxy are blocked.
* **Exposing Local Services**: To let the agent reach a local database or local LLM (e.g. Ollama on `127.0.0.1:11434`), specify the port using `-allow-port 11434` or the `allow_ports` configuration. A secure loopback forwarder will bind `127.0.0.1:11434` inside the sandbox and proxy traffic to the host.
