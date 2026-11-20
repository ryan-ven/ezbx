# `ezbx init` flow (draft 0.1)

`ezbx init` is the one command that takes a new user from nothing to a working box. This page describes what it asks, what it checks, what it writes and what happens when something goes wrong. Naming: the command and files are `ezbx`, the brand is EZBox.

## Principles

1. **Show the plan before changing anything.** Every run ends with a review screen listing exactly what will be created, downloaded and changed. Nothing happens until you confirm.
2. **Safe defaults.** Network off, one writable folder, no extra folders exposed, no keys on disk.
3. **Never touch what it didn't create.** Every Docker resource EZBox makes carries a label, and EZBox only ever changes labelled resources.
4. **Re-runnable.** Running `init` again reads the existing setup, shows what would change, and offers to add a harness or switch engines. It never silently replaces your config or regenerates secrets.
5. **Same questions without prompts.** Every prompt has a flag, so the whole flow can run unattended.
6. **No telemetry.** The only network traffic is what Docker needs to pull images and what each harness's install needs.

## Run modes

```text
ezbx init                 guided, with prompts
ezbx init --plan          show what it would do; change nothing
ezbx init --yes           accept detected defaults without prompting
ezbx init --config FILE   take every answer from a config file
```

Useful flags: `--engine`, `--harness a,b`, `--workspace PATH`, `--network off|allow|on`, `--no-start`, `--here` (write config next to the current folder, for developers).

Prompts are plain text questions in version 0.1. Arrow-key menus can come later without changing the flow.

## The steps

### 0. Preflight

Checks, in this order, each with a plain-language fix if it fails:

- The operating system and architecture are supported.
- The Docker command exists, and the Docker engine answers.
- Enough free disk space for the images (the exact threshold is decided once image sizes are known).
- The chosen workspace parent folder exists and is writable.

If the Docker engine isn't running, `init` says so and offers to wait while you start it. It does not start other programs without asking. It also does not install Docker.

### 1. Engine

1. Run the adapters' detection (see `engine-adapters.md`) and show what was found:

   ```text
   Inference engines found on this computer
     1. Strata      ready, 1 model
     2. LM Studio   not running
   Use which? [1]
   ```

2. If one is found, it's the default answer. If several, you choose. If none, you may pick "I'll start one later" and `init` finishes with a note, because the box works without a running model and the gateway just reports "engine not reachable".
3. If an engine can be started by its adapter (`control.start`), offer to do so. It always asks first.
4. If the engine requires a key, ask for the **name of an environment variable** that holds it. The key is never typed into a file. (The operating system's credential store comes after 0.1; decided below.)

### 2. Harnesses

A checklist grouped by kind (coding tools first, assistant-style tools after).

- Each line shows its pinned version and its `verified` state. Unverified ones are marked and not preselected.
- Pi is preselected while it is the only verified-in-practice option.
- If a chosen harness needs the internet (a cloud-backed tool), the review screen reminds you that it won't work while the network is `off`.

### 3. Folders

1. **Writable workspace.** Default is a folder inside the user's home, shown with the exact path. It is created if missing. These are refused with a clear reason: a drive root, the home folder itself, a system folder, or a folder that already contains EZBox's own config or state.
2. **Read-only folders (optional, none by default).** If you add some, `init` scans them and lists any files that look like secrets (environment files, key files, credential files) and offers to mask them. Exposing a whole drive is allowed but gets an explicit warning that anything readable could be sent out if the network is later turned on.

### 4. Network mode

Default `off`. A one-line explanation of each mode is shown. If `allow` is chosen, the allowlist is built from the chosen harnesses' domains plus the small default list, and shown for review.

### 5. Limits

Memory defaults to **half of the memory Docker has**, rounded down to whole gigabytes, at least 2 GB and at most 8 GB, so the box can't take over a gaming or work session. On Docker Desktop the box runs inside Docker's own virtual machine, so its memory (`docker info`), not the computer's, is what counts. There is no CPU limit by default, and the process limit is 512. All are editable, and the review screen shows them.

### 6. Review

```text
EZBox plan (nothing has been changed yet)

Engine        Strata (detected)
Harnesses     Pi 1.5.0 (observed in a prior setup, will be re-checked on first build)
Workspace     <workspace path>   (will be created)
Read-only     none
Network       off
Memory        8g

Will create
  2 Docker networks, 1 volume, 2 containers, 1 image set (build needs internet once)
  config file       <config folder>/ezbx.toml
  state folder      <state folder>   (logs, snapshots, admin token)

Continue? [y/N]
```

### 7. Apply

In order, each step recorded so a failed run can resume:

1. Write the config and lock file.
2. Create the networks and volume, all labelled.
3. Build the base image and add each chosen harness's install layer.
4. Generate each harness's config from the engine's model list.
5. Create and start the gateway, then the box.
6. Run `ezbx doctor`.
7. Print what to do next, such as `ezbx shell pi`.

If a step fails, `init` stops, explains in plain words, and leaves the earlier steps in place. Running `init` again resumes from the failed step. `ezbx destroy` removes everything EZBox created (never your workspace) if you want a clean slate.

## Where files go

| Kind | Location | Notes |
|---|---|---|
| Config | per-user config folder | `ezbx.toml` |
| State | per-user state folder, one folder per box | logs, snapshots, admin token. Never inside the workspace and never mounted into the box. |
| Lock | next to the config | `ezbx.lock`, pinned versions |
| Workspace | chosen in step 3 | the only writable mount |

`--here` keeps config next to the current folder instead, which suits developers working from a clone of the repo.

The folders per operating system are design decision 9 in [design.md](design.md): `%LOCALAPPDATA%\ezbx` on Windows, `~/Library/Application Support/ezbx` on macOS, and the XDG config and state folders on Linux. State is private to the user and kept out of any mounted folder.

## Docker resource naming

All names start with `ezbx-` and include the box name, for example `ezbx-main-gateway`, `ezbx-main-box`, `ezbx-main-inside`, `ezbx-main-home`. Every resource also has a label (`ezbx.managed=true`) so tools can find exactly what belongs to EZBox.

## Failure and edge cases

| Situation | Behavior |
|---|---|
| Docker missing | Explains how to install it. Does not install it. |
| Docker engine stopped | Offers to wait while you start it. |
| No engine found | Continues if you choose, with a clear note. |
| Engine only listening on loopback outside Docker Desktop | `doctor` explains the limitation (design decision 4). |
| Offline machine | Build steps fail clearly. Everything not needing the network is still done. |
| Harness install fails | The harness is skipped, the others continue, and the failure is shown. |
| Existing setup | Shows a diff of what would change and asks first. |

## Decided here

- **Plain prompts first**, with every question also available as a flag (closes open question 2 in `design.md`).
- **Config and state live in per-user folders, not in the repo and not in the workspace** (closes open question 3).

- **Keys come from environment variables only in 0.1.** `init` asks for the name of the variable. Using the operating system's credential store (a different interface on each system) comes later.
- **The default memory limit** is half of Docker's memory, rounded down to whole gigabytes, from 2 to 8 GB (step 5).
- **`init` never starts Docker Desktop.** It says Docker isn't running, offers to wait, and carries on when Docker answers. Starting it would need to know where Docker Desktop is installed on each system.
- **`init --plan` makes no network calls.** It lists which images are already on the computer and which would be downloaded. Whether a download works shows when the plan is applied.

## Open questions

None at the moment.
