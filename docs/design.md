# EZBox design (draft 0.1)

Status: first draft. Most of this is a proposal. Built so far: engine detection, the gateway (model route, egress proxy, admin API), config rendering for Pi, `ezbx doctor`, `ezbx up`/`down`, which create the networks, the gateway and the box from `ezbx.toml`, `ezbx shell`, `ezbx net` and `ezbx engine use` for a running box, `ezbx init`, the guided setup, and `ezbx build` with `ezbx.lock` and tool packs ([build.md](build.md)). Items marked **verify** depend on facts I still need to confirm against current documentation before building.

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

The inside network must be created with `--internal` **and** the bridge option `gateway_mode_ipv4=isolated` (Docker Engine 28 or later), with IPv6 off. A plain internal network still gives its bridge an address that belongs to Docker Desktop's VM, and the box could reach services listening there. With the option, a test probe on Docker Desktop reached the gateway's model port and nothing else: no internet, host, LAN, Docker Desktop addresses, other containers or outside DNS. The same held from inside the first box image, where Pi reached the model through the gateway. The method, results and limits are in [isolation-test.md](isolation-test.md). Note that the box can reach every port the gateway container listens on, so anything that must stay private to the host cannot listen on the gateway's network interfaces. The admin API therefore listens only on the gateway container's own loopback and is reached with `docker exec` (decision 7; see [gateway.md](gateway.md), section 2).

## 3. The gateway

One small service that does four jobs:

| Job | Detail |
|---|---|
| **Model route** | Exposes one stable OpenAI-compatible address inside the box, for example `http://gateway:8000/v1`, and forwards to whichever engine is selected. Switching engines changes one setting and no harness config. |
| **Key injection** | Keys stay on the host and the agent never sees a real one. Built for the engine's key on the model route. Cloud API keys will use one reverse route per provider on the gateway (decision 8, not built yet), because the egress proxy never decrypts HTTPS and so cannot add a key to a tunnelled request. |
| **Egress control** | A forward proxy that applies the current network mode (section 5). |
| **Request log** | Records model, latency and how each request ended. Token counts and blocked requests are planned. |

Decided: the gateway is one small Go binary, shipped in a minimal image and sharing code with the CLI. The full design is in [gateway.md](gateway.md).

## 4. Engine adapters

Each engine has a small adapter folder, `providers/<id>/`, with an `adapter.json` file. The format is described in [engine-adapters.md](engine-adapters.md), and `providers/strata/adapter.json` is a complete example.

The adapter does the following:
- **Detect:** probe known local ports and report what's running. Default ports differ per engine, so they get verified at implementation time (**verify**).
- **Discover:** read the model list from `/v1/models`, so no model name is ever hardcoded.
- **Control (optional):** start the engine, stop it, or unload the model to free the GPU. Engines without a documented control interface are simply "detected, not controlled".

Engines planned for the first set: Strata, LM Studio, Ollama and llama.cpp's server. All are expected to expose an OpenAI-compatible API (**verify** per engine).

### Host reachability

Containers reach the host through a host alias. On Docker Desktop this worked for an engine bound to loopback in a private predecessor setup. It has not been tested with EZBox yet. On other setups it may not work. The gateway owns this detail, and `ezbx doctor` tests it.

## 5. Network modes

| Mode | Model route | Internet |
|---|---|---|
| `off` | yes | none |
| `allow` | yes | only domains on the allowlist (package registries, the code host, and any API a chosen cloud harness needs) |
| `on` | yes | the public internet (local networks and the host stay blocked) |

- Switching modes only changes the gateway's rules, so running sessions continue.
- A mode switched with `ezbx net` lasts until the next `ezbx up` or gateway restart, which go back to the mode in `ezbx.toml`, so a forgotten `on` never outlives its session. Hosts approved with `ezbx net allow` are saved in the box's state folder and kept (decision 11).
- Each harness manifest lists the domains it needs, and `allow` merges the lists of the installed harnesses.
- Cloud-backed harnesses won't work in `off`. `ezbx doctor` says so.
- The default allowlist is tiny: package registries and the code host. Everything else is added on demand. The gateway logs blocked requests, and `ezbx net allow <domain>` approves one. Allowlists are plain data files, so people can contribute to them.

## 6. The box

- **User:** an unprivileged `agent` user. No `sudo`, no setuid tricks, `no-new-privileges` on, all Linux capabilities dropped.
- **Writable:** only the workspace folder you choose (`/workspace`) plus the agent's own home volume.
- **Optional read-only view:** you may expose extra host folders read-only. Common secret files are masked with empty overlays, and the `ezbx` scanner warns about other secret-like files in anything exposed.
- **No Docker socket**, ever.
- **Resource limits:** memory, CPU and process limits with sensible defaults, adjustable.
- **Admin access:** reserved for the human. `ezbx admin shell` starts a separate, short-lived admin container from the same image, because root in the main box has no capabilities. It mounts the workspace and the agent's home, has no network unless asked, and nothing else in it lasts: programs for the agent come from the tool catalogue and `ezbx build`, so images stay pinned. See [admin.md](admin.md).

Future option: a read-only root filesystem with tmpfs for scratch space.

## 7. Harnesses

Each harness is a folder, `harnesses/<id>/`, with a `manifest.json` and an install script. The format is described in [harness-manifest.md](harness-manifest.md), and `harnesses/pi/manifest.json` is a complete example.

- **Verified:** nothing ships without a recorded install check.
- **Pinned:** versions are pinned and recorded in `ezbx.lock`, so a rebuild gives the same box (see [build.md](build.md)).
- **Tools:** programs such as ripgrep, Chromium or FFmpeg come from a catalogue of verified tools and tool packs, chosen in `ezbx.toml` and built into the image. The agent can't install programs itself.
- **Config generation:** the harness template is filled from the selected engine adapter's model list, so each harness points at the gateway automatically.
- **Candidates:** Pi, oh-my-pi (a separate project from Pi), OpenCode, Codex CLI, Claude Code, Hermes Agent and OpenClaw. Daily-driver agents such as Hermes Agent and OpenClaw may need extra services or different runtimes, so they come after the coding harnesses.
- **Autonomy flag:** each harness's "skip approvals" mode is set only inside the box and is documented per harness (**verify** names against current docs).

## 8. Snapshots and rewind

Before each session, EZBox snapshots the workspace into a store in the box's state folder, outside the box's mounts, so the agent can't edit its own history. `ezbx rewind` restores a snapshot (the newest by default) after taking one of the current state, so a rewind can be undone too. `ezbx review` shows what changed since a snapshot. Built; see [snapshots.md](snapshots.md).

## 9. Command line

```text
ezbx init               guided setup: engine, harnesses, folders
ezbx up | down          start or stop the gateway and box
ezbx shell [harness]    open a terminal in the box, or launch a harness
ezbx net off|allow|on   change the network mode
ezbx engine detect|use|status
ezbx harness add|remove|list
ezbx snapshot | rewind | review
ezbx admin shell        human-only root shell in a temporary admin container
ezbx doctor             check Docker, engine, gateway, mounts and network
ezbx reset              a fresh agent home and containers; backs up first and refuses to continue if the backup fails
ezbx destroy            remove everything EZBox made for a box, never the workspace
```

Language: Go, built as one cross-platform binary. The first milestone targets Windows with Docker Desktop, and the code avoids Windows-only assumptions so Linux can follow.

## 10. Configuration and privacy

- `ezbx.toml.example`: committed, placeholders only.
- `ezbx.toml`: your real config, in the per-user config folder (decision 9), outside the repo. Unknown settings are refused (decision 10).
- `ezbx.local.toml`: gitignored, for a config kept inside a clone of the repo (`--here`, later).
- `ezbx.lock`: committed. Pinned harness and image versions.
- Secrets live in environment variables on the host in 0.1 (the host's credential store later), never in the repo or the image.
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

## 13. Decisions

| # | Question | Decision |
|---|---|---|
| 1 | Language | Go, for the CLI and the gateway. One static binary, easy cross-compiling, and a standard library that covers the proxy work. |
| 2 | Allowlist | Tiny default list plus each harness's domains. Blocked requests are logged and approved with `ezbx net allow`. Lists are data files. |
| 3 | Platforms | Windows with Docker Desktop first. No Windows-only assumptions in the code. Linux next, macOS later. |
| 4 | Engines bound to loopback outside Docker Desktop | Version 0.1 supports Docker Desktop only. `ezbx doctor` detects the problem and explains the fix. A proper helper comes later. |
| 5 | Boxes | One box in 0.1 holding many projects. The config format allows several named boxes so they can be added later without breaking anything. |
| 6 | Second harness | Pi first, then OpenCode. OpenCode still needs its local-endpoint support and install method verified before it is committed to. |
| 7 | Reaching the admin API | It listens only on the gateway container's own loopback, with no published port. The CLI calls it through `docker exec`, and a token is still required. The box has no network path to it. See [gateway.md](gateway.md), section 2. |
| 8 | Cloud API keys | One reverse route per cloud provider on the gateway, built like the model route. The harness points its base address at the gateway, and the gateway adds the key and forwards to that provider's fixed address. Each provider gets a small adapter for its key style. Only API-key providers are covered; account logins that refresh their own tokens are a separate case for later. Built after the isolation proof, since 0.1's first harness uses a local model. |
| 9 | Where config and state live | Windows: `%LOCALAPPDATA%\ezbx` holds `ezbx.toml`, and `%LOCALAPPDATA%\ezbx\state\<box>` holds the admin token and logs (snapshots later). Not the Roaming folder, which a domain profile copies between machines; the token, the logs and the machine's own paths should stay on this one. macOS: `~/Library/Application Support/ezbx`, the same layout. Linux: `$XDG_CONFIG_HOME/ezbx` and `$XDG_STATE_HOME/ezbx` (`~/.config/ezbx` and `~/.local/state/ezbx`). `EZBX_HOME` moves both, for tests. The state folder is never mounted into the box, and the workspace may not overlap it. On Windows the token's file mode means little; what protects it is that the user's profile folder is private to that user and to administrators. |
| 10 | Config format | TOML, read with [BurntSushi/toml](https://github.com/BurntSushi/toml) (MIT, no dependencies of its own), EZBox's first outside module, pinned in `go.sum`. Unknown settings are refused, so a typo is an error. Sections: `[box.<name>]`, `[engine]` with `[engine.<id>]` overrides, `[models."<id>"]` for facts engines don't report, and `[gateway]`. A parser of our own could replace the module later if we want no dependencies. |
| 11 | Changes to a running box | `ezbx net off\|allow\|on` and `ezbx engine use` change the running gateway only. The next `ezbx up`, or a restart of the gateway (Docker Desktop restarting, say), goes back to `ezbx.toml`: the safe default wins, so a forgotten `net on` doesn't survive. Hosts approved with `ezbx net allow HOST` are kept: they are saved in `approved-hosts` in the box's state folder, which is mounted read-only into the gateway (never the box) and read with `--allow-file` each time it starts. `ezbx net remove HOST` forgets one. |
| 12 | Where the image build files come from | Carried inside the `ezbx` program: the Dockerfiles, harness install files, the tool catalogue and the gateway's Go source. `ezbx build` needs no copy of the repository. See [build.md](build.md). |
| 13 | `ezbx.lock` and what `up` accepts | `ezbx.lock` is committed and generated from the files: base image digests, harness versions and lockfile hashes, tools. What was built on a computer goes in `images.json` next to `ezbx.toml`. `up` refuses images not built from the current inputs. |
| 14 | Image names and old images | Each image is named after a hash of its inputs (`ezbx-box:<12 hex digits>`). Rebuilds keep the old images; `ezbx build --prune` removes older ones after asking, keeping the current images and the ones they replaced. |
| 15 | Snapshots | A store of EZBox's own in the state folder (content kept once, compressed, by hash; a list per snapshot), not git, which keeps a nested git repository only as a pointer. Taken at every `ezbx shell` and by hand. Rebuildable folders and files over 100 MB left out; the last 20 automatic ones kept. `rewind` snapshots first, then matches the workspace exactly, and refuses while a session runs. See [snapshots.md](snapshots.md). |
| 16 | Admin shell | A temporary root container from the box image, with Docker's normal capabilities (never privileged), no Docker socket, the workspace and agent home mounted, home `/root`, no network unless `--net`. Nothing outside the mounts lasts; programs come from tools and `ezbx build`. See [admin.md](admin.md). |
| 17 | `reset` and `destroy` | `reset` backs up the config, approved hosts and the agent's home (refusing if that fails), then gives a fresh home and containers; workspace, snapshots, approved hosts, config and images are kept; `--restore ID` undoes it. `destroy` removes the containers, networks, home volume, state folder (snapshots too) and EZBox's images, never the workspace, keeps `ezbx.toml` unless `--remove-config`, and asks you to type the box name. See [admin.md](admin.md). |

## 14. Open questions

1. ~~Exact format of `ezbx.toml`.~~ Decided: decision 10, and [ezbx.toml.example](../ezbx.toml.example).
2. ~~How `ezbx init` presents the choices.~~ Decided in [init-flow.md](init-flow.md): plain prompts first, every question also a flag.
3. ~~Where snapshots and the gateway log live on disk by default.~~ Decided in [init-flow.md](init-flow.md): per-user config and state folders, never inside the workspace.
4. How the gateway proves which engine and model it is forwarding to, so `doctor` can confirm the path end to end. (The `X-Ezbx-*` reply headers are a first answer; see [gateway.md](gateway.md).)
5. ~~How cloud API keys are injected.~~ Decided: a reverse route per cloud provider on the gateway (decision 8).
