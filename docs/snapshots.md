# Snapshots, `rewind` and `review`

A snapshot is an undo point for the box's workspace. `ezbx shell` takes one before every session, so whatever an agent does in the workspace can be looked at and undone.

```text
ezbx snapshot [-m NOTE]        take one by hand (kept until you remove it)
ezbx snapshot list             every snapshot, with its kind, time, files, size and note
ezbx snapshot remove ID        remove one (asks first; --yes doesn't)
ezbx review [ID] [--diff]      what changed since a snapshot (the newest by default)
ezbx rewind [ID] [--yes]       put the workspace back as it was (the newest by default)
ezbx shell --no-snapshot       start a session without taking one
```

## Decisions

Decided before the code was written (design decision 15).

| Question | Decision |
|---|---|
| How snapshots are stored | A small store of EZBox's own, in the box's state folder (`state\<box>\snapshots`), which is never mounted into the box, so the agent can't change its own history. Each file's content is kept once, compressed and named by its SHA-256 hash; each snapshot is a list of paths, sizes, times, modes and hashes, written last, so a crash never leaves half a snapshot. No git is needed on the computer. A git repository was ruled out: a project folder that is a git repository itself was kept only as a pointer to its last commit, not its files (tested). This store keeps such projects in full, `.git` folder included, so even an agent's `git reset --hard` or rewritten history can be undone. |
| When snapshots are taken | At the start of every `ezbx shell`, with or without a harness, and by hand with `ezbx snapshot`. If the snapshot fails, the session doesn't start (`--no-snapshot` starts it anyway). |
| What is left out, and how many are kept | Folders and files named `node_modules`, `.venv`, `venv`, `__pycache__`, `.pytest_cache`, `.mypy_cache`, `.next` or `.cache`, at any depth, and files over 100 MB. Everything left out is listed. The last 20 automatic snapshots are kept; snapshots taken by hand are kept until removed; content no snapshot uses is freed. |
| What `rewind` does | First a snapshot of the workspace as it is (kind `rewind`), so the rewind can be undone by rewinding to that one. Then the workspace is made to match the chosen snapshot: changed and deleted files come back, and files and folders made since are removed. What the snapshot left out is never touched. It refuses while anything besides the box's own idle process runs in the box, shows what it will change and asks first. |
| What `review` shows | The changed, deleted and new files since a snapshot; `--diff` adds the changed lines of text files. |

## Settings

In `ezbx.toml`, all optional:

```toml
[box.main.snapshots]
skip     = ["node_modules", ".venv", "*.log"]   # replaces the default list; [] leaves out nothing
max_file = "100m"                               # larger files are left out
keep     = 20                                   # automatic snapshots kept
```

`skip` holds names, not paths, matched against every folder's and file's own name at any depth. `*` and `?` work as wildcards.

## How it works

- **Taking a snapshot** walks the workspace without following links (a link is saved as a link). A file with the same size and time as in the previous snapshot is taken to be unchanged, so a repeat snapshot of a large workspace only reads what changed. Two files with the same content are stored once.
- **`review` and `rewind` read every file in full.** A change that keeps a file's size and time exactly (as `touch -d` can) would pass the shortcut above, so the commands that decide what changed don't use it. An automatic snapshot can miss such a change; it is still found and undone against the snapshot before it.
- **`rewind` writes on this computer, as you**, so it guards the way: every path in a snapshot must be a plain path inside the workspace (no `..`, no absolute path, and on Windows no `\` or `:`), and it refuses to write through a folder that is a link, because an agent could replace a folder with a link to somewhere else and wait for a rewind. A rewound file is written to a temporary file in the same folder, then moved into place, with its mode and time.
- **Files inside a `.git` folder** are git's own bookkeeping. They are snapshotted and rewound like any other file, but `review` and `rewind` sum them up in one line per project ("git history: app/.git (git's own files: 5 changed, 3 new)") instead of listing them.
- **A lock file** in the store keeps two `ezbx` commands from changing it at once. A lock left by a command that crashed more than an hour ago is taken over.

## Limits

- A snapshot taken while an agent writes (for example, a second `ezbx shell` already running) can catch a file in the middle of a change. `rewind` avoids this by refusing while a session runs; `ezbx snapshot` and the automatic snapshot don't check.
- Folders and files left out are not undone. Remove the `node_modules` folder, for example, and install again.
- On Windows, creating links needs Developer Mode or administrator rights. A link in a snapshot that can't be made again is reported as not done.
