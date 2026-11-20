# EZBox design (draft 0.1)

Status: first draft. Everything here is a proposal, and nothing is built yet. Items marked **verify** depend on facts I still need to confirm against current documentation before building.

## 1. Goal

Make it easy to run AI agent tools in a sandbox while the model itself keeps running on your own machine.

- **Power on the host.** The inference engine uses your GPU and models directly.
- **Work in the box.** Agents run in a container and write only to folders you choose.
- **Easy.** One setup flow, a small command-line tool, and a switch for network access.

### Non-goals (for now)

- Running the model inside the container.
- Being a hardened, multi-tenant security product. The box reduces risk. It doesn't remove it. See the threat model.
- Supporting every agent tool. Each harness is added only after its install method is verified.

## 2. Architecture

```text
  Host (your PC)                           Docker
 ┌─────────────────────────┐   ┌────────────────────────────────────────────┐
 │ Inference engine        │   │  "inside" network  (no internet route)     │
 │  Strata / LM Studio /   │   │  ┌──────────┐        ┌──────────────────┐  │
 │  Ollama / llama.cpp     │◄──┼──┤ gateway  │◄───────┤ box (harnesses)  │  │
 │                         │   │  └────┬─────┘        └──────────────────┘  │
 └─────────────────────────┘   │       │ "outside" network (internet)       │
                               └───────┼────────────────────────────────────┘
                                       ▼
                                   the internet (only through the gateway)
```

Three parts:

1. **Engine adapters** (host side): find and control the inference engine.
2. **Gateway** (container): the only way out of the box. It carries model traffic and, depending on the network mode, internet traffic.
3. **Box** (container): where harnesses run, as an unprivileged user.

### Why the network is enforced by topology

The box sits only on an internal Docker network, which has no route to the internet or to the host. The gateway sits on that network and on a normal one. If the agent ignores proxy settings, it still has no other route. Environment variables alone could be changed by the agent. Network layout can't.

## 3. The gateway

One small service that does four jobs:

| Job | Detail |
|---|---|
| **Model route** | Exposes one stable OpenAI-compatible address inside the box, for example `http://gateway:8000/v1`, and forwards to whichever engine is selected. Switching engines changes one setting and no harness config. |
| **Key injection** | Cloud API keys stay on the host. The gateway adds them to outgoing requests, so the agent never sees a real key. |
| **Egress control** | A forward proxy that applies the current network mode (section 5). |
| **Request log** | Records model, token counts, latency and blocked requests, for debugging and for judging a model. |

Open question: gateway language and shape. Leading option is one small Go binary, shipped in a minimal image, reusing the CLI's code.

## 4. Engine adapters

Each engine has a small adapter folder, `providers/<id>/`, with a manifest like this:

```json
{
  "id": "example-engine",
  "name": "Example Engine",
  "detect": { "ports": [1234], "probe": "/v1/models" },
  "openaiBase": "/v1",
  "capabilities": ["chat", "tools", "vision"],
  "control": { "start": null, "stop": null, "unload": null }
}
```

The adapter does the following:
- **Detect:** probe known local ports and report what's running. Default ports differ per engine, so they get verified at implementation time (**verify**).
- **Discover:** read the model list from `/v1/models`, so no model name is ever hardcoded.
- **Control (optional):** start the engine, stop it, or unload the model to free the GPU. Engines without a documented control interface are simply "detected, not controlled".

Engines planned for the first set: Strata, LM Studio, Ollama and llama.cpp's server. All are expected to expose an OpenAI-compatible API (**verify** per engine).

### Host reachability

Containers reach the host through a host alias. On Docker Desktop this worked for an engine bound to loopback. On other setups it may not. The gateway owns this detail, and `ezbox doctor` tests it.

## 5. Network modes

| Mode | Model route | Internet |
|---|---|---|
| `off` | yes | none |
| `allow` | yes | only domains on the allowlist (package registries, the code host, and any API a chosen cloud harness needs) |
| `on` | yes | everything |

- Switching modes only changes the gateway's rules, so running sessions continue.
- Each harness manifest lists the domains it needs, and `allow` merges the lists of the installed harnesses.
- Cloud-backed harnesses won't work in `off`. `ezbox doctor` says so.

## 6. The box

- **User:** an unprivileged `agent` user. No `sudo`, no setuid tricks, `no-new-privileges` on, all Linux capabilities dropped.
- **Writable:** only the workspace folder you choose (`/workspace`) plus the agent's own home volume.
- **Optional read-only view:** you may expose extra host folders read-only. Common secret files are masked with empty overlays, and the `ezbox` scanner warns about other secret-like files in anything exposed.
- **No Docker socket**, ever.
- **Resource limits:** memory, CPU and process limits with sensible defaults, adjustable.
- **Admin access:** reserved for the human. `ezbox admin shell` starts a separate, short-lived admin container from the same image, because root in the main box has no capabilities. Admin tasks such as system packages happen there and are baked in on rebuild.

Future option: a read-only root filesystem with tmpfs for scratch space.

## 7. Harnesses

Each harness is a folder, `harnesses/<id>/`, with a manifest and an install script.

```json
{
  "id": "example-harness",
  "name": "Example Harness",
  "kind": "coding",
  "install": { "type": "npm", "spec": "example-harness", "version": "1.2.3" },
  "binary": "example",
  "config": { "path": "~/.example/config.json", "template": "config.json.tmpl" },
  "autonomyFlag": "--skip-approvals",
  "network": { "domains": [] },
  "verified": { "on": "YYYY-MM-DD", "version": "1.2.3" }
}
```

- **Verified:** nothing ships without a recorded install check.
- **Pinned:** versions are pinned and recorded in `ezbox.lock`, so a rebuild gives the same box.
- **Config generation:** the harness template is filled from the selected engine adapter's model list, so each harness points at the gateway automatically.
- **Candidates:** Pi, oh-my-pi (a separate project from Pi), OpenCode, Codex CLI, Claude Code, Hermes Agent and OpenClaw. Daily-driver agents such as Hermes Agent and OpenClaw may need extra services or different runtimes, so they come after the coding harnesses.
- **Autonomy flag:** each harness's "skip approvals" mode is set only inside the box and is documented per harness (**verify** names against current docs).

## 8. Snapshots and rewind

Before each session, EZBox snapshots the workspace into a repository that lives outside the box's mounts, so the agent can't edit its own history. `ezbox rewind` restores the last snapshot. `ezbox review` shows what changed since it.

## 9. Command line

```text
ezbox init               guided setup: engine, harnesses, folders
ezbox up | down          start or stop the gateway and box
ezbox shell [harness]    open a terminal in the box, or launch a harness
ezbox net off|allow|on   change the network mode
ezbox engine detect|use|status
ezbox harness add|remove|list
ezbox snapshot | rewind | review
ezbox admin shell        human-only root shell in a temporary admin container
ezbox doctor             check Docker, engine, gateway, mounts and network
ezbox reset              rebuild from scratch; backs up config first and refuses to continue if the backup fails
```

Language: one cross-platform binary (Go is the leading option). The first milestone targets Windows with Docker Desktop, and the code stays portable.

## 10. Configuration and privacy

- `ezbox.toml.example`: committed, placeholders only.
- `ezbox.local.toml`: gitignored. Holds machine paths and choices.
- `ezbox.lock`: committed. Pinned harness and image versions.
- Secrets live in the host's credential store or a gitignored env file, never in the repo or the image.
- The repo has a pre-commit and CI privacy scan. Docs use placeholder paths such as `C:\Users\Example`.

## 11. Threat model (summary, to expand)

The box protects against:
- Agent changes outside the writable folder.
- Unrequested internet access (when the mode is `off` or `allow`).
- Agents holding real cloud API keys.
- Accidents that need an undo (snapshots).

It does not fully protect against:
- A container escape through a kernel or runtime bug, because containers share a kernel with their host.
- Anything you mount read-write or expose read-only. The agent can read what you expose, and could send it out if the network mode allows.
- A malicious model or harness working within the allowed network and folders.

## 12. Milestones

1. **0.1:** `init`, engine detection for Strata and one other engine, the gateway with the model route and `off`/`on`, one harness, snapshots.
2. **0.2:** `allow` mode, more engines and harnesses, `doctor`, `reset` with backup and restore.
3. **0.3:** the remaining harnesses, `review`, starter profiles (web dev, Python/ML, research, offline).
4. **Later:** tray or GUI front end, other container runtimes, a tool-calling self-test for models.

## 13. Open questions

1. Gateway and CLI language (Go vs another).
2. Allowlist format and defaults, and who maintains them.
3. Whether the first release is Windows-only or includes Linux and macOS.
4. How to handle engines that bind only to loopback on non-Desktop setups.
5. Per-project boxes in 0.1, or a single box.
6. Which harness goes second, after the first.
