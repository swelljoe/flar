# flar

FLAR is the Fast Light Agent Restrictor. It runs on rocks called gars.

It is a simple, lightweight CLI tool in Go to run coding agent CLIs (like Claude Code, Antigravity, Codex, and Copilot) safely inside isolated `podman` containers.

## Features

- **Auto-Detection Heuristics**: Automatically detects project environments (Go, Rust, Python, TypeScript, Perl) by scanning key files (e.g., `go.mod`, `Cargo.toml`, `pyproject.toml`) and parsing `.gitignore` patterns.
- **Agent Sandbox**: Runs the agent inside a rootless container mapping the current workspace as `/workspace` with SELinux support (`:Z`).
- **Dangerous Bypass Options**: Automatically injects flags (like `--dangerously-skip-permissions` for Claude and `agy`, or `--dangerously-bypass-approvals-and-sandbox` for Codex) so agents run without runtime approval interruptions. Can be disabled with `--ask`.
- **Config Copying**: Automatically copies host credentials (like `~/.claude/`, `~/.codex/`, `~/.gemini/`, or GitHub CLI configurations) to a temporary directory mounted read-write inside the container so the host config files remain untouched.
- **Custom Templates**: Allows users to specify project-local (`.flar/Containerfile`) or user-global (`~/.config/flar/templates/`) Containerfiles for custom environments.

## Build and Install

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

- `-m`: Specify the agent to run (`claude`, `codex`, `agy`, `copilot`). Defaults to checking available directories or environment variables.
- `-t`: Override the language template (`go`, `rust`, `python`, `typescript`, `perl`, `generic`).
- `-ask`: Do not skip permissions/approvals (forcing the agent to ask for permission).
- `-rebuild`: Force rebuild the podman container image to update agent and packages.
- `-v`: Enable verbose logging.

If you only use one agent and your project only has one obvious language, you can run:

```
flar
```

Inside your project directory.

## Custom Containerfiles

FLAR allows you to define your own Containerfiles to customize the build environment. This is useful if you want to include extra CLI tools, libraries, or system packages for the agent to use.

### File Resolution Order

When building the image for a given project language (`<lang>`) and agent (`<agent>`), FLAR searches for custom files in the following order:

1. **Project-local exact override**: `<project>/.flar/Containerfile`
2. **Project-local language & agent match**: `<project>/.flar/<lang>.<agent>.Containerfile`
3. **Project-local language match**: `<project>/.flar/<lang>.Containerfile`
4. **User-global language & agent match**: `~/.config/flar/templates/<lang>.<agent>.Containerfile`
5. **User-global language match**: `~/.config/flar/templates/<lang>.Containerfile`
6. **User-global generic & agent match**: `~/.config/flar/templates/generic.<agent>.Containerfile`
7. **User-global generic fallback**: `~/.config/flar/templates/generic.Containerfile`
8. **Built-in template fallback**: Internal defaults.

### Writing a Custom Containerfile

The ideal custom Containerfile would:
1. Use a matching base image (e.g. `registry.fedoraproject.org/fedora:44` to align with the host), so you and the agent are working with the same tools.
2. Install git, compilers, runtimes, and the target agent CLI.
3. Set the working directory to `/workspace` so that mounted files are in the expected path.

#### Example: Project-local Python with Claude Code (`.flar/Containerfile`)

Here is an example `.flar/Containerfile` for a Python project that uses Claude Code and requires `sqlite-devel`, `tmux`, and `htop`:

```dockerfile
FROM registry.fedoraproject.org/fedora:44

# Install basic toolchain and dependencies
RUN dnf install -y python3 python3-pip python3-uv python3-ty gcc make sqlite-devel git ripgrep fd-find findutils coreutils tar unzip procps-ng && dnf clean all

# Install Node.js & npm (required for Claude Code)
RUN dnf install -y nodejs npm && dnf clean all

# Install additional diagnostics/utilities
RUN dnf install -y tmux htop vim && dnf clean all

# Preinstall the agent
RUN npm install -g @anthropic-ai/claude-code

# Set workspace directory
WORKDIR /workspace
```

