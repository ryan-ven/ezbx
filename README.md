# EZBox

**Local models, contained agents.**

EZBox is a planned one-click setup for running AI coding and agent tools inside a sandboxed container, while the model itself keeps running on your own machine.

> **Status: early development.** One harness (Pi) works end to end on Windows with Docker Desktop: `ezbx init` sets it up from nothing, builds the images and starts the box, and `ezbx shell pi` opens it. Undo (snapshots), the admin shell and `reset`/`destroy` are not built yet; the checklist below shows where things stand. Test results are in [docs/isolation-test.md](docs/isolation-test.md), and designs are in [docs/](docs/).

```text
go install ./cmd/ezbx     from a clone; needs Go and Docker Desktop
ezbx init                 asks the questions, shows the plan, builds and starts the box
ezbx shell pi             opens Pi in the box
```

Other commands: `ezbx up` and `ezbx down` start and remove the box described in `ezbx.toml` (see [ezbx.toml.example](ezbx.toml.example)), `ezbx build` builds the images, `ezbx net` shows and switches the network and approves hosts, `ezbx engine detect` and `ezbx engine use` find an engine and point the box at it, and `ezbx doctor` checks the whole setup and explains any fix.

## The idea

- **The model stays on your machine.** Your inference engine (Strata, LM Studio, Ollama or llama.cpp) keeps full access to your GPU and models.
- **The work stays in the box.** Agents run in a container and can only write to folders you choose.
- **One stable address between them.** A small gateway lets the box talk to whichever engine you run, so switching engines doesn't mean reconfiguring every tool.
- **You choose the harnesses.** Pick from agent tools such as Pi, OpenCode, Codex CLI, Claude Code, Hermes Agent and OpenClaw, in any combination. Install methods will be verified before each one is added.
- **Network on or off.** A switch controls whether the agent can reach the internet.
- **Root stays with you.** The agent runs as an unprivileged user. Admin access is reserved for the human.

## Planned first version

1. An `ezbx init` setup that detects running engines and asks which harnesses to install.
2. The gateway and the network switch.
3. Snapshots of the writable folder, so an agent session can be undone.

## Road to the first usable version

The goal: set up Pi with a local model on Strata, from nothing, using only `ezbx` commands, with every planned safety feature. Other harnesses, other engines and cloud keys come after.

| Command | What it does | Status |
|---|---|---|
| `ezbx init` | Finds the engine, asks the questions, writes `ezbx.toml`, shows the plan first | done |
| `ezbx build` | Builds the images from pinned versions listed in `ezbx.lock`, with the tool packs you chose ([docs/build.md](docs/build.md)) | done |
| `ezbx up` / `ezbx down` | Start and remove the box | done |
| `ezbx shell` / `ezbx shell pi` | A terminal in the box, or Pi | done |
| `ezbx net off\|allow\|on`, `ezbx net allow HOST` | The network switch and approvals | done |
| `ezbx engine use` | Point a running box at an engine, for example after it moves to a new port | done |
| `ezbx snapshot` / `rewind` / `review` | Undo an agent's changes to the workspace | not built |
| `ezbx admin shell` | A root shell for the human, in a separate container | not built |
| `ezbx reset` / `destroy` | Start again, or remove everything EZBox made | not built |
| `ezbx doctor` | Check the whole setup and explain fixes | done |

## Safety note

A container is a strong boundary but not a perfect one. It shares a kernel with its host, and anything you mount into it can be changed by the agent. A full threat model will be written before the first release.

## Contributing and security

Contributions will open once the design settles. To report a security problem, see [SECURITY.md](SECURITY.md).

## License

MIT. Copyright (c) 2026 Ryan Ven.
