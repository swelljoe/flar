# flar

FLAR is the Fast Light Agent Restrictor. It runs on rocks called gars.

It is a simple, lightweight CLI tool in Go to run coding agent CLIs (like Claude Code, Antigravity, Codex, Copilot, Reasonix, Kimi Code, Poolside, Qwen Code, and omp) safely inside isolated [Bubblewrap (`bwrap`) sandboxes](https://github.com/containers/bubblewrap).

![Antigravity CLI riding in a flar](/assets/agy-in-a-flar.png)

The purpose is to *instantly* and without complicated configuration, bubblewrap an AI agent so it only has access to the project you're working on. This protects against prompt injections as well as supply chain issues in libraries the agent might pull into your project without sufficient vetting (or just bad luck). The only accessible sensitive information is the agent's own auth details and chat history for the project.

Most agents have a "sandbox" feature, but it is quite porous and the agent itself can expand the scope of what's accessible. And, of course, supply chain vulnerabilities are not subject to the agent sandbox. `flar` is wholly impervious to the agent, and the blast radius of supply chain attacks tightly constrained.

Bubblewrap is extremely well-tested, and actively maintained. It is used by Flatpack and many other projects for lightweight containers. `flar` is far less well-tested, and used by me for a couple of days.

## Features

- **Bubblewrap Sandbox**: Runs the agent in an unprivileged user namespace using a clean root directory (`tmpfs`). System paths (`/usr`, `/bin`, `/lib`, `/lib64`, etc.) are mounted read-only from the host, ensuring host packages are immediately available without container image management. The host's OpenSSL configuration is parsed for `.include` directives and the targets are mounted too — on Fedora and RHEL that pulls in `/etc/crypto-policies`, without which OpenSSL fails to initialize and Node-based agents refuse to start.
- **Strict Filesystem Isolation**: Only the target project directory is bind-mounted read-write. The rest of the host home directory is hidden, protecting ssh keys, shell configurations, and personal files from prompt injection attacks.
- **Network Sandboxing**: 
  - **Isolated Mode (Default)**: The network namespace is unshared. Internet access is tunneled through a host-side HTTP/HTTPS proxy that performs DNS lookup on the host and blocks traffic to loopback, private (RFC 1918/CGNAT), link-local (including the `169.254.169.254` cloud metadata endpoint), and multicast addresses. The proxy dials the IP it validated (no DNS re-resolution) and never follows redirects internally, so a public URL cannot bounce the agent to a blocked address.
  - **Port Forwarding**: Selectively expose local services (e.g. databases, llama.cpp models) into the sandbox by mapping specific ports to the host's `localhost`.
  - **Host Mode**: Option to share the host's network namespace for unconstrained access.
- **Dangerous Bypass Options**: Automatically injects flags (like `--dangerously-skip-permissions` for Claude/`agy` or `--dangerously-bypass-approvals-and-sandbox` for Codex) so agents run without runtime approval interruptions. Can be disabled with `-ask`. For Kimi Code, `--yolo` is omitted automatically when `-p`/`--prompt` is passed, since `kimi` rejects that combination. For Qwen Code, `--yolo` is injected unless `-ask` is set.
- **Config Copying**: Automatically copies host credentials (like `~/.claude/`, `~/.codex/`, `~/.gemini/`, or GitHub CLI configurations) to a temporary directory mounted inside the sandbox home directory, leaving host config files untouched.
- **Session Persistence & Resume**: When reasonably safe (currently Claude Code, Reasonix, and Qwen Code) conversations started inside a sandbox are written back to the host, so `--resume`/`--continue` works across runs — scoped to the current project so no other project's history enters the sandbox. Otherwise, history is forked on first run of `flar` for a given agent and project (Codex, Copilot, Kimi Code, Pool, omp, and Antigravity). See [Session persistence & resume](#session-persistence--resume).
- **Keyring Bridging (`agy`)**: The Antigravity CLI stores its OAuth token in the OS keyring rather than a file. `flar` extracts only that one secret and serves it inside the sandbox through a private, in-process Secret Service — so the agent authenticates without exposing the rest of your keyring. See [Credentials](#credentials).
- **Container Builds (podman)**: A wrapped agent can use rootless `podman`/`buildah` from the host's `/usr` to pull images, build images, and run containers inside the sandbox. Storage is ephemeral by default; `-container-cache` persists it under flar's own cache directory. See [Container builds (podman)](#container-builds-podman).

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
go build -o `flar` .
```

To install:

```bash
mv `flar` ~/.local/bin/
```

## Usage

Run `flar` in your project folder or specify the path:

```bash
flar [flags] [path/to/project] [extra agent args/prompts...]
```

### Flags

- `-m`: Specify the agent to run (`claude`, `codex`, `agy`, `copilot`, `reasonix`, `kimi`, `pool`, `qwen`, `omp`). Defaults to checking available host configurations or environment variables.
- `-ask`: Do not skip permissions/approvals (forcing the agent to ask for permission).
- `-network`: Network mode: `isolated` (default) or `host`.
- `-allow-port`: Allow a specific local TCP port (e.g. `8080`, `11434`) through the isolated network sandbox. Can be specified multiple times.
- `-container-cache`: Persist container images built or pulled by podman inside the sandbox under `~/.cache/flar/containers` (respecting `XDG_CACHE_HOME`). Without it, container storage is ephemeral and discarded on exit. See [Container builds (podman)](#container-builds-podman).
- `-v`: Enable verbose logging.

### Configuration file (`.flar.json`)

You can configure options per-project in `<project>/.flar.json` or globally in `~/.config/flar/config.json`:

```json
{
  "agent": "claude",
  "ask": false,
  "network": "isolated",
  "allow_ports": [5432, 11434],
  "container_cache": false
}
```

The following additional options are available in the **global** config only:

* `omp_allowed_models`: An array of omp provider IDs (e.g. `["anthropic", "openai", "google"]`) to restrict which provider API key environment variables are forwarded into an omp sandbox. See [omp auth model allowlist](#omp-auth-model-allowlist).

A project-level `.flar.json` travels with the repository, so it cannot weaken sandbox isolation: `network`, `allow_ports`, and `container_cache` are honored **only** from the global config (or the `-network` / `-allow-port` / `-container-cache` flags) and are ignored — with a warning — when found in a project config. Otherwise a malicious repo could turn on host networking, open local ports, or add host directories the sandbox may write to, for anyone who clones it and runs `flar`. `agent` and `ask` are safe to set per-project. `omp_allowed_models` is also global-only, to prevent a malicious repo from overriding your allowlist and exposing additional API keys.

## Credentials

Because only a temporary copy of your config is mounted, agents run authenticated using your existing host session without touching the originals. Most agents keep their session in files that `flar` copies directly:

* **Claude**: `~/.claude/` (including `.credentials.json`) **and** `~/.claude.json`, the top-level file holding onboarding state and account identity. Both are required; with only the credentials, Claude treats the sandbox as a fresh install and prompts for login.
* **Codex / Copilot**: `~/.codex/`, `~/.copilot/`, and the GitHub CLI config.
* **Kimi Code**: `~/.kimi-code/` — `config.toml`, `tui.toml`, and `device_id`. Kimi stores credentials in files (`storage = "file"`), so no keyring bridge is needed. Unlike the other agents, the credential dirs (`credentials/`, `oauth/`) are **live-bound from the host** rather than copied into flar's persistent shadow home: Kimi's access tokens live only ~15 minutes and the refresh token rotates on every use, so a copied token goes stale almost immediately, and a sandbox-side refresh of a copied token would invalidate the host's login (and vice versa). Live-binding means `kimi login` on the host is picked up immediately, at the cost of the sandbox being able to write to Kimi's own credential files (which hold only its OAuth tokens, readable by an authenticated agent anyway). The `bin/` (the `kimi` executable itself) and `updates/` directories are not copied; the real binary is bind-mounted read-only at run time.
* **Poolside (`pool`)**: `~/.config/poolside/` — `credentials.json`, `settings.yaml`, and `skills/`. Pool stores API keys in `credentials.json` (and also honors the `POOLSIDE_API_KEY` environment variable), so no keyring bridge is needed. The config is copied to a temporary directory; the state directory (`~/.local/state/poolside/`) is forked per-project (see below).
* **Qwen Code**: `~/.qwen/` — `settings.json` (which holds API keys under its `env` key), `output-language.md`, `extensions/`, `skills/`, `memories/`, and `installation_id`. Qwen also honors the `DASHSCOPE_API_KEY`, `BAILIAN_CODING_PLAN_API_KEY`, and `BAILIAN_TOKEN_PLAN_API_KEY` environment variables. No keyring bridge is needed.
* **omp**: `~/.omp/agent/` — `config.yml`, `models.yml`, `agent.db` (auth store), and `blobs/` (content-addressed blobs). omp authenticates via many different provider API keys (Anthropic, OpenAI, Gemini, Groq, DeepSeek, etc.), which `flar` forwards as environment variables into the sandbox. The `sessions/` and `history.db` paths are excluded from the config copy and handled by the per-project shadow store (see below). The `agent.db` auth store and `blobs/` directory are safe to copy as-is since they contain no cross-project data.

### omp auth model allowlist

omp is an open-harness agent that supports many different model providers, each with its own API key environment variable. By default, `flar` forwards all known provider API key env vars into the sandbox. This is convenient but means every provider's key is visible to the sandboxed agent.

To limit which providers' keys are exposed, set `omp_allowed_models` in the **global** config (`~/.config/flar/config.json`):

```json
{
  "omp_allowed_models": ["anthropic", "openai", "google"]
}
```

When set, `flar` forwards only the env vars for the specified provider IDs (e.g. `ANTHROPIC_API_KEY` for `anthropic`, `OPENAI_API_KEY` for `openai`, `GEMINI_API_KEY` for `google`). This prevents prompt injection from reading API keys for providers you don't intend to use with omp.


### Antigravity (`agy`) keyring

`agy` is the exception: it does **not** store its token in a file. It keeps it in the OS keyring, read via the freedesktop Secret Service API over the D-Bus session bus. The sandbox has no session bus, so a naïve setup fails with `authentication failed or timed out`.

flar handles this specially:

1. On the host, it extracts **only** the `agy` token (keyring item `service=gemini, username=antigravity`) using `secret-tool`, and writes it to a `0600` file in the temporary config directory.
2. Inside the sandbox, it runs a minimal, self-contained Secret Service (`flar --internal-secretsvc`) on a private Unix socket, pointed to by `DBUS_SESSION_BUS_ADDRESS`. It serves that single token and nothing else.

The agent can reach exactly its own token — not the rest of your keyring (browser passwords, other apps' secrets, etc.). The implementation speaks the D-Bus wire protocol directly, so it needs **no** `gnome-keyring` or `dbus-daemon` inside the sandbox.

Requirements and caveats:

* Host-side extraction needs `secret-tool` (libsecret) installed on the host. If it is absent or the token is not found, `flar` skips the bridge and `agy` falls back to its normal login prompt.
* Any authenticated agent can, by definition, read its own token; a prompt-injection attack could exfiltrate it. This is inherent to running authenticated at all. The keyring bridge limits the exposure to that one token rather than your entire keyring.

## Session persistence & resume

Because the sandbox mounts a temporary *copy* of your config, anything an agent writes there would normally vanish on exit — including the conversation it just had. `flar` binds each agent's transcript storage back to the host so sessions persist and can be resumed later, while keeping other projects' history out of the sandbox.

* **Claude**: transcripts live in a per-project directory (`~/.claude/projects/<project-slug>/`). `flar` binds only the current project's directory from the host over the copied config, so `claude --resume` sees this project's sessions and nothing else. Beware this means a prompt injection stored in history might still be a risk, if you resume a session that contains a working prompt injection, the blast radius gets infinitely larger if you run `claude` outside of `flar`. I think the convenience outweighs the risk, for any projects that have that kind of risk, just always run it in `flar`.

* **Codex CLI**: Codex stores transcripts under date-based directories in `~/.codex/sessions/` and indexes them in the global `state_5.sqlite`; both record each thread's `cwd`, but the on-disk directories mix projects. `flar` gives each workspace a shadow Codex home under `$XDG_STATE_HOME/flar/codex/<project-slug>/`, falling back to `~/.local/state/flar/codex/<project-slug>/` when that variable is unset. On first use it seeds that home with only matching transcript files, SQLite rows, and prompt-history entries. The shadow home is then mounted as `~/.codex`, so new sessions persist without exposing another project to `codex resume --all`.

* **Copilot CLI**: Copilot stores resumable sessions in two global places under `~/.copilot/`: a SQLite index (`session-store.db`) and one directory per session under `session-state/<session-id>/`. The SQLite rows carry the owning `cwd`, and each state directory is keyed by the same session ID. Binding the host store as-is would expose every project's sessions to a sandboxed Copilot, so `flar` gives each workspace its own shadow Copilot home:

```
~/.copilot/.flar/<project-slug>/
```

On first use, `flar` seeds that shadow home with only the sessions whose stored `cwd` matches the current workspace, copying both the relevant SQLite rows and the matching `session-state/` directories. After that, the whole shadow home is bind-mounted as `~/.copilot` inside the sandbox, so `copilot --continue` can resume only this workspace's sessions, and new sessions persist there safely.

* **Antigravity (`agy`)**: as with Copilot, sessions are "forked" on first run in a project by `flar`, and are no longer shared with `agy` running outside of `flar`.

`agy` does not separate conversations by project on disk. Every conversation for every project lives in one flat store under `~/.gemini/antigravity-cli/` (`conversations/`, `brain/`, `implicit/`), keyed only by a UUID, with the owning workspace recorded *inside* opaque conversation blobs. A recency index (`cache/last_conversations.json`) maps each workspace to its most recent conversation, which is what `agy --continue` follows.

Binding that store into the sandbox as-is would let a sandboxed `agy` resume — via `--continue`, the interactive picker, or an explicit `--conversation <ID>` — a conversation belonging to a *different* project, leaking whatever was pasted into it. To prevent that, `flar` gives each workspace its own **scoped store**:

```
~/.gemini/antigravity-cli/.flar/<project-slug>/
```

This directory is bind-mounted over `conversations/`, `brain/`, `implicit/`, `history.jsonl`, and `cache/last_conversations.json` inside the sandbox. A sandbox opened on project A can therefore only ever see project A's conversations. New sessions accumulate in the scoped store and are resumable on the next run.

The first time `flar` runs `agy` in a project, it seeds that project's scoped store from your existing host history — but only with conversations `agy` itself attributes to this workspace (determined from the plain-text `last_conversations.json` and `history.jsonl`, never by parsing the conversation blobs). After that one-time seed the scoped store is independent: new sessions live only in the scoped store, and later host-side changes are not pulled in.

* **Reasonix**: Just like Claude Code. Reasonix keeps each project history in its own subdirectory directory, so it can be bindmounted in the same way as Claude Code, with the same caveat.

* **Qwen Code**: Just like Claude Code and Reasonix. Qwen keeps each project's sessions in its own subdirectory (`~/.qwen/projects/<project-slug>/chats/`), so it can be bind-mounted in the same way, with the same caveat. The project slug uses the same encoding as Claude — replacing every non-alphanumeric character with `-`.

* **Kimi Code**: as with Codex and Copilot, sessions are "forked" on first run in a project by `flar`, and are no longer shared with `kimi` running outside of `flar`.

Kimi Code splits resume state between per-workspace transcript directories (`~/.kimi-code/sessions/<workspace-id>/<session-id>/`, each self-attributing via the `workDir` in its `state.json`) and a global `session_index.jsonl`, which `kimi --continue` / `--session` needs in order to load a session at all. The index mixes every project's sessions (as absolute paths), and `workspaces.json` lists the root of every workspace ever opened, so neither can be bound into the sandbox as-is. Instead, `flar` gives each workspace a shadow Kimi home:

```
$XDG_STATE_HOME/flar/kimi/<project-slug>/
```

(falling back to `~/.local/state/flar/kimi/<project-slug>/`). On first use it seeds that home with only the sessions attributed to the current workspace — read from each session's own `state.json` and from the host index, never from session contents — plus a scoped `session_index.jsonl`, this project's prompt-history file (`user-history/<md5(workDir)>.jsonl`), and a `workspaces.json` filtered down to this project. The whole shadow home is then bind-mounted as `~/.kimi-code` (with the host's `credentials/` and `oauth/` live-bound over it, as described in [Credentials](#credentials)), so new sessions persist there and `kimi --continue` can resume only this workspace's sessions.

* **Poolside (`pool`)**: as with Codex and Copilot, sessions are "forked" on first run in a project by `flar`, and are no longer shared with `pool` running outside of `flar`.

Pool splits resume state between two global directories that mix every project and a per-project directory:

* `~/.local/state/poolside/sessions/` — one `session-<uuid>.json` metadata file per session. These files carry no workspace information themselves.
* `~/.local/state/poolside/trajectories/` — one `trajectory-standalone_<uuid>.ndjson` per session, whose first line is a `session.start` event containing a `working_directories` array of absolute paths. This is the only on-disk record of which workspace a session belongs to.
* `~/.local/state/poolside/pool/<project-slug>/` — per-project prompt history (`prompt-history.json`) and logs (`logs/<project-slug>/<session-id>/`). These are already scoped by project slug.

Binding the global `sessions/` and `trajectories/` directories into the sandbox as-is would let a sandboxed `pool` resume a session belonging to a *different* project via `pool -r` / `--resume`, leaking whatever was pasted into it. To prevent that, `flar` gives each workspace its own **shadow state directory**:

```
$XDG_STATE_HOME/flar/pool/<project-slug>/
```

the sandbox (with the config copy bind-mounted at `~/.config/poolside`), so new sessions persist there and `pool -r` / `--resume` can resume only this workspace's sessions.

* **omp**: as with Codex and Copilot, sessions are "forked" on first run in a project by `flar`, and are no longer shared with `omp` running outside of `flar`.

omp stores sessions under `~/.omp/agent/sessions/` in project-scoped subdirectories (keyed by a path encoding of the workspace), but its on-disk layout can vary across versions and a single directory may contain sessions from multiple projects (e.g. after a cwd migration). The session header line (first or second line, depending on format version) contains a `cwd` field that `flar` reads to attribute each session to a workspace. omp also maintains a centralized SQLite database (`~/.omp/agent/history.db`) with a `history` table (`id`, `prompt`, `created_at`, `cwd`, `session_id`) and an `history_fts` FTS5 index that mixes all projects.

Binding the host store into the sandbox as-is would let a sandboxed `omp` resume a session belonging to a *different* project, leaking whatever was pasted into it. To prevent that, `flar` gives each workspace its own **shadow store**:

```
~/.omp/agent/.flar/<project-slug>/
```

On first use it seeds that shadow store with only the sessions attributed to the current workspace (read from each session's header `cwd` field) and a filtered `history.db` containing only rows whose `cwd` matches the current workspace (using `VACUUM INTO` + `DELETE` + `VACUUM`, the same pattern as Codex). The `sessions/` and `history.db` paths are excluded from the config copy; the shadow store is bind-mounted as `~/.omp/agent` inside the sandbox. New sessions persist there and `omp --continue` can resume only this workspace's sessions. After the one-time seed, wrapped and unwrapped omp histories are independent, like the other forked agents.

The agent directory can be relocated via the `PI_CODING_AGENT_DIR` environment variable; `flar` honors this when locating the host store.

Consequences to be aware of:

* **Copilot CLI**: as with `agy`, the scoped shadow home becomes a separate per-project world after the initial seed. Sessions you start with Copilot **outside** `flar` are not pulled into `flar` later, and sessions you start **inside** `flar` are saved back to the scoped shadow home rather than the host's global Copilot store.
* Sessions you start with `agy` **outside** `flar` are (aside from the initial seed) not visible **inside** flar, and vice versa. This is deliberate — flar's `agy` history is a separate, per-project world.
* Codex follows the same rule: after the one-time seed, wrapped and unwrapped Codex histories are independent.
* Kimi Code likewise: after the one-time seed, wrapped and unwrapped Kimi histories are independent. Host-side changes to `config.toml` are not picked up after the seed either — delete the project's shadow home under `$XDG_STATE_HOME/flar/kimi/` to re-seed. (Credentials are the exception: they are live-bound, so `kimi login` and token refreshes on either side are seen by both.)
* Pool follows the same rule: after the one-time seed, wrapped and unwrapped Pool histories are independent. Delete the project's shadow home under `$XDG_STATE_HOME/flar/pool/` to re-seed.
* omp follows the same rule: after the one-time seed, wrapped and unwrapped omp histories are independent. Delete the project's shadow store under `~/.omp/agent/.flar/` to re-seed.
* A conversation that `agy`'s own indices never attributed to the current workspace is not seeded, by design. The safe default is to withhold it rather than risk exposing another project's data.
* Agent-owned `.flar/` directories are excluded from config copies for compatibility, and flar-owned state lives under `$XDG_STATE_HOME/flar/` (or `~/.local/state/flar/`), outside agent-managed configuration directories.

## Network Security & Local Ports

In **isolated** network mode, the agent's environment has no direct access to host network interfaces.

* **Internet Access**: Works automatically for HTTP/HTTPS requests (such as connecting to cloud LLMs like Anthropic or Gemini) using the `HTTP_PROXY` and `HTTPS_PROXY` environment variables.
* **Localhost Restrictions**: Requests to `localhost`, loopback, private, and link-local IPs via the proxy are blocked (this includes cloud instance metadata endpoints such as `169.254.169.254`).
* **Exposing Local Services**: To let the agent reach a local database or local LLM (e.g. Ollama on `127.0.0.1:11434`), specify the port using `-allow-port 11434` or the `allow_ports` setting in the **global** config. A secure loopback forwarder will bind `127.0.0.1:11434` inside the sandbox and proxy traffic to the host.

## Container builds (podman)

The sandbox is prepared so a wrapped agent can use rootless `podman`/`buildah` (from the host's `/usr`, no separate installation inside the sandbox) to pull images, build images with `podman build`, and run containers — subject to the constraints of the isolation:

* **Ephemeral by default**: images and layers live on the sandbox's tmpfs and are discarded on exit. Pass `-container-cache` (or set `container_cache: true` in the **global** config) to persist them under `~/.cache/flar/containers` (respecting `XDG_CACHE_HOME`). The cache is deliberately *not* podman's usual `~/.local/share/containers` location, so it never mixes with host-side podman use, and it is obvious which directory the sandbox is allowed to write. Note that image layers are arbitrary content pulled from registries: treat the cache directory accordingly. To delete the cache, use `chmod -R u+w ~/.cache/flar/containers && rm -rf ~/.cache/flar/containers` — rootless podman storage contains read-only layer directories that a plain `rm -rf` cannot remove.
* **Single-UID mapping**: bwrap maps only your user into the sandbox, so nested containers run in rootless single-mapping mode — file ownership inside images is squashed to your UID. flar supplies the `ignore_chown_errors` overlay setting this mode requires.
* **Host signature policy required**: podman 5 refuses to pull images without a signature policy, and flar will not generate an accept-anything one on the host's behalf — that would silently disable image signature verification. If the host ships `/etc/containers`, its policy and registry configuration are inherited verbatim; if it ships no policy, flar warns at startup and image pulls inside the sandbox fail until the host provides one (usually via the `containers-common` package).
* **Container networking**: `podman run`'s default private (pasta) networking cannot work inside the sandbox — the kernel refuses to open `/dev/net/tun` from an unprivileged user namespace. Use `podman run --network=host` (the container then shares the sandbox's own network mode) or `--network=none`. `podman build` RUN steps are unaffected.
* **No resource limits**: there is no usable cgroup hierarchy inside the sandbox, so nested containers have no CPU/memory limits — the same as the sandboxed agent itself. If you need a ceiling, wrap the whole `flar` invocation, e.g. `systemd-run --user --scope -p MemoryMax=8G -p TasksMax=2000 flar ...`.
* **Isolated network mode**: image pulls and RUN-step downloads go through flar's proxy like everything else (`HTTP_PROXY`/`HTTPS_PROXY` are honored by podman/buildah), so only public hosts are reachable. Use `-network host` if builds need direct network access.
