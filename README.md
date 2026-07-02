# flar

FLAR is the Fast Light Agent Restrictor. It runs on rocks called gars.

It is a simple, lightweight CLI tool in Go to run coding agent CLIs (like Claude Code, Antigravity, Codex, and Copilot) safely inside isolated Bubblewrap (`bwrap`) sandboxes.

![Antigravity CLI riding in a flar](/assets/agy-in-a-flar.png)

## Features

- **Bubblewrap Sandbox**: Runs the agent in an unprivileged user namespace using a clean root directory (`tmpfs`). System paths (`/usr`, `/bin`, `/lib`, `/lib64`, etc.) are mounted read-only from the host, ensuring host packages are immediately available without container image management.
- **Strict Filesystem Isolation**: Only the target project directory is bind-mounted read-write. The rest of the host home directory is hidden, protecting ssh keys, shell configurations, and personal files from prompt injection attacks.
- **Network Sandboxing**: 
  - **Isolated Mode (Default)**: The network namespace is unshared. Internet access is tunneled through a host-side HTTP/HTTPS proxy that performs DNS lookup on the host and filters out traffic to local/loopback IP addresses.
  - **Port Forwarding**: Selectively expose local services (e.g. databases, llama.cpp models) into the sandbox by mapping specific ports to the host's `localhost`.
  - **Host Mode**: Option to share the host's network namespace for unconstrained access.
- **Dangerous Bypass Options**: Automatically injects flags (like `--dangerously-skip-permissions` for Claude/`agy` or `--dangerously-bypass-approvals-and-sandbox` for Codex) so agents run without runtime approval interruptions. Can be disabled with `-ask`.
- **Config Copying**: Automatically copies host credentials (like `~/.claude/`, `~/.codex/`, `~/.gemini/`, or GitHub CLI configurations) to a temporary directory mounted inside the sandbox home directory, leaving host config files untouched.
- **Keyring Bridging (`agy`)**: The Antigravity CLI stores its OAuth token in the OS keyring rather than a file. flar extracts only that one secret and serves it inside the sandbox through a private, in-process Secret Service — so the agent authenticates without exposing the rest of your keyring. See [Credentials](#credentials).

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

## Credentials

Because only a temporary copy of your config is mounted, agents run authenticated using your existing host session without touching the originals. Most agents keep their session in files that flar copies directly:

* **Claude**: `~/.claude/` (including `.credentials.json`) **and** `~/.claude.json`, the top-level file holding onboarding state and account identity. Both are required; with only the credentials, Claude treats the sandbox as a fresh install and prompts for login.
* **Codex / Copilot**: `~/.codex/`, `~/.copilot/`, and the GitHub CLI config.

### Antigravity (`agy`) keyring

`agy` is the exception: it does **not** store its token in a file. It keeps it in the OS keyring, read via the freedesktop Secret Service API over the D-Bus session bus. The sandbox has no session bus, so a naïve setup fails with `authentication failed or timed out`.

flar handles this specially:

1. On the host, it extracts **only** the `agy` token (keyring item `service=gemini, username=antigravity`) using `secret-tool`, and writes it to a `0600` file in the temporary config directory.
2. Inside the sandbox, it runs a minimal, self-contained Secret Service (`flar --internal-secretsvc`) on a private Unix socket, pointed to by `DBUS_SESSION_BUS_ADDRESS`. It serves that single token and nothing else.

The agent can reach exactly its own token — not the rest of your keyring (browser passwords, other apps' secrets, etc.). The implementation speaks the D-Bus wire protocol directly, so it needs **no** `gnome-keyring` or `dbus-daemon` inside the sandbox.

Requirements and caveats:

* Host-side extraction needs `secret-tool` (libsecret) installed on the host. If it is absent or the token is not found, flar skips the bridge and `agy` falls back to its normal login prompt.
* Any authenticated agent can, by definition, read its own token; a prompt-injection attack could exfiltrate it. This is inherent to running authenticated at all. The keyring bridge limits the exposure to that one token rather than your entire keyring.

## Network Security & Local Ports

In **isolated** network mode, the agent's environment has no direct access to host network interfaces.

* **Internet Access**: Works automatically for HTTP/HTTPS requests (such as connecting to cloud LLMs like Anthropic or Gemini) using the `HTTP_PROXY` and `HTTPS_PROXY` environment variables.
* **Localhost Restrictions**: Requests to `localhost` or loopback IPs via the proxy are blocked.
* **Exposing Local Services**: To let the agent reach a local database or local LLM (e.g. Ollama on `127.0.0.1:11434`), specify the port using `-allow-port 11434` or the `allow_ports` configuration. A secure loopback forwarder will bind `127.0.0.1:11434` inside the sandbox and proxy traffic to the host.
