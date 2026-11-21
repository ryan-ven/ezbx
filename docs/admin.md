# `ezbx admin shell`, `ezbx reset` and `ezbx destroy`

Three commands for the human, never the agent.

```text
ezbx admin shell [--net] [-- COMMAND...]     a root shell beside the box
ezbx reset [--rebuild] [--restore ID] [--yes] [--no-doctor]
ezbx reset --list                            the backups reset has made
ezbx destroy [--remove-config] [--yes]       remove everything EZBox made for the box
```

## Decisions

Decided before the code was written (design decisions 16 and 17).

| Question | Decision |
|---|---|
| What the admin container gets | The box image, run as root with Docker's normal capabilities (so apt and chown work), never `--privileged`, no Docker socket. The same workspace (`/workspace`) and agent home (`/home/agent`) as the box, so paths match. Its own home is `/root`, so nothing the agent left in its home (a `.bashrc`, say) runs as root. Removed when it exits; the box keeps running. One at a time per box. |
| The admin shell's network | None by default. `--net` connects it to the gateway's outside network, for downloads: the internet, not the box's network switch. |
| What lasts | Changes in `/workspace` and the agent's home last; everything else is gone at exit, and the shell says so when it starts. Programs for the agent come from `tools` in `ezbx.toml` and `ezbx build`, not from the admin shell, so images stay pinned and named by their inputs. `-- COMMAND` runs one command instead of bash. |
| What `destroy` removes | The box's containers (and an open admin shell), its networks, the agent's home volume, the box's state folder (admin token, logs, approved hosts, all snapshots, backups), EZBox's images and `images.json`. Never the workspace. `ezbx.toml` is kept unless `--remove-config` is given. Images another container uses (another box) are kept, and so are base images that aren't EZBox's (`node`, `golang`). |
| How `destroy` confirms | It lists everything first, with sizes and the number of snapshots and backups, then asks you to type the box name. `--yes` skips that, but refuses while a session runs. |
| What `reset` does | A fresh agent home and fresh containers with the same settings: the containers, networks and home volume are removed, then `ezbx up` runs again. Kept: the workspace, snapshots, approved hosts, config and images. `--rebuild` also builds both images again without Docker's cache. |
| `reset`'s backup | Before anything changes: `ezbx.toml`, `approved-hosts` and the home volume as `home.tar`, in `state\<box>\backups\<time>\`. If any part fails, nothing is changed. The tar is read back in full and must end with tar's end marker, so a copy cut off anywhere is caught. The newest 5 backups are kept. `--restore ID` puts a backup's `home.tar` into the fresh home volume, so a reset can be undone. It asks first, and refuses while a session or an admin shell runs. |

`destroy --config` was the first name for removing `ezbx.toml` too; it became `--remove-config`, because every command already uses `--config FILE` to mean "this config file".

## How it works

- **Admin shell.** `docker run --rm` with the box's labels plus `ezbx.role=admin`, named `ezbx-<box>-admin`, `--user 0:0`, `no-new-privileges`, a process limit of 512, `HOME=/root`, and `--network none` (or the outside network with `--net`). The home volume must already exist and be EZBox's, because `docker run` would otherwise quietly make an unlabelled one; `ezbx up` makes it.
- **Backup and restore of the home** use a throwaway container from the box image with no network: `tar --numeric-owner -C /home/agent -cf - .` from a read-only mount, and `tar --numeric-owner -xpf -` into the new volume, so owners and modes come back exactly. The stream goes straight to and from the file, never through memory.
- **What is "running"** for `reset` and `destroy`: anything in the box besides its own idle process (as for `rewind`), or an open admin shell.
- **Destroy's state folder** is removed only after checking that it lies inside the state root and doesn't hold or sit inside the workspace.

## Limits

- An admin shell with `--net` reaches the whole internet; the box's network switch doesn't apply to it.
- `destroy` on one box removes EZBox images no container uses, even if another box that is down would need them. `ezbx build` makes them again.
- A backup holds the agent's home, the config and the approved hosts, not the snapshots (they stay where they are) or the images (they can be built again).
