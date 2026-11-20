# EZBox

**Local models, contained agents.**

EZBox is a planned one-click setup for running AI coding and agent tools inside a sandboxed container, while the model itself keeps running on your own machine.

> **Status: early design. Nothing here is usable yet.** This repository currently holds the project skeleton, the license and the privacy checks. The sections below describe what I intend to build, not what exists.

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

## Safety note

A container is a strong boundary but not a perfect one. It shares a kernel with its host, and anything you mount into it can be changed by the agent. A full threat model will be written before the first release.

## Contributing and security

Contributions will open once the design settles. To report a security problem, see [SECURITY.md](SECURITY.md).

## License

MIT. Copyright (c) 2026 Ryan Ven.
