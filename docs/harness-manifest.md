# Harness manifest (schema 1, draft)

A harness is an agent tool that runs inside the box. Each lives in `harnesses/<id>/` with a `manifest.json` and an install script. The manifest is plain data, so adding a harness never needs a code change.

## Fields

| Field | Required | Meaning |
|---|---|---|
| `schema` | yes | Manifest format version. Currently `1`. |
| `id` | yes | Short lowercase name, also the folder name. |
| `name` | yes | Display name. |
| `kind` | yes | `coding` or `assistant`. Used to group harnesses in `ezbx init`. |
| `install` | yes | How it is installed (see below). The version must be pinned. |
| `binary` | yes | Command name that must exist after install. |
| `runtime` | yes | `node`, `python` or `binary`. Tells the image builder which toolchain to include. |
| `tools` | no | Tool ids from the catalogue (`catalog/tools/`) the harness always needs, built into the box with it whatever the config chooses. Pi needs `fd` and `ripgrep`. See [build.md](build.md). |
| `env` | no | Environment variables set for this harness. Never secrets. |
| `config` | no | Config files to generate (see below). |
| `launch` | yes | Command and working directory used by `ezbx shell <id>`. |
| `autonomy` | yes | The flag that skips approval prompts, or `null` with a note. Only ever used inside the box. |
| `network` | yes | Domains the harness needs in `allow` mode. `optional` groups are off unless chosen. |
| `secrets` | yes | Names of secrets it can use. Values are never stored here. Usually empty, because the gateway injects keys. |
| `verified` | yes | What has actually been tested (see below). |

### `install`

```json
{ "type": "npm", "package": "name", "version": "1.2.3", "script": "install.sh", "lockfile": "package-lock.json" }
```

- `type`: `npm`, `pip`, `binary` or `script`.
- `lockfile`: required for `npm`. The lockfile in the harness folder.
- `version` is always exact, such as `1.2.3` or `1.2.3-beta.1`. Ranges, tags and `latest` are refused when manifests are loaded, so a rebuild gives the same box.
- Pinning the harness alone does not pin what it pulls in. An npm harness therefore ships a `package.json` and a `package-lock.json` in its folder. The lockfile pins every package it pulls in with an integrity hash, and the install uses `npm ci`, which refuses anything that doesn't match. `ezbx.lock` records the lockfile's own hash (see [build.md](build.md)).
- `script` runs at image build time as an **unprivileged** user, in a separate build stage, and must fail loudly if the binary is missing. The result is copied into the image owned by root, so the agent can run the harness but not change it.
- Package install scripts are off (`--ignore-scripts`). A harness that really needs one must say which and why in its install script.

### `config`

Each entry creates one file inside the box, owned by the unprivileged `agent` user.

```json
{ "path": "~/.tool/config.json", "mode": "0600", "content": { ... } }
```

- `path` starts with `~/` (the agent's home) and has no empty, `.` or `..` parts, so a file can't land outside the home.
- `mode` is four octal digits. A config file is never executable or writable by others.

`content` is ordinary JSON with placeholders, so the output is always valid JSON and no text templating is needed.

**Placeholders**

| Placeholder | Value |
|---|---|
| `${gateway.baseUrl}` | The gateway's OpenAI-compatible address as seen from inside the box. |
| `${gateway.key}` | A placeholder key for the gateway. Not a real credential. |
| `${models.default.id}` | The model chosen as default. |

A value that is exactly one placeholder keeps its type, so a number stays a number and a list stays a list. A placeholder inside a longer string is substituted as text.

**Repeating a block for each model**

```json
"models": { "$each": "models", "as": { "id": "${id}", "contextWindow": "${contextWindow}" } }
```

This produces a list with one entry per model the engine reports. A `$each` block has exactly the keys `$each` and `as`, repeats only over `models`, and can't be nested.

**Model facts available inside `as`**

| Name | Source |
|---|---|
| `id` | Read from the engine's model list. |
| `name` | The id by default. Can be overridden with `name` under `[models."<id>"]` in `ezbx.toml`. |
| `contextWindow` | Read from the engine when it reports one, otherwise a configured default (32768). |
| `maxTokens` | A configured default (8192), never more than the context size. |
| `reasoning` | A configured default (off), because engines don't report it. A reasoning model needs it on, and enough `maxTokens` for its reasoning and the answer. |
| `input` | What the engine reports, otherwise `["text"]`. |

**Errors, not guesses.** An unknown placeholder, a model fact used outside `$each`, a list placed inside a longer string, an unknown `$` key, a placeholder in a key name, or an unfinished `${` all stop rendering with a message. Nothing is ever left unfilled in a file inside the box. If the engine reports no models, there is nothing to configure yet, and rendering says so.

### Rendering

`ezbx harness render <id> --out FOLDER` finds the engine (or uses `--url`), fills in the templates and writes the files under `FOLDER`, laid out like the agent's home (for Pi, `FOLDER/.pi/agent/models.json` and `settings.json`). `--default`, `--context`, `--max-tokens` and `--reasoning` set the choices above. `ezbx init` and `ezbx harness sync` will call the same code.

Tested: Pi's rendered config, with no hand edits, worked inside the box against a real model on Strata, including its read and write tools (see [isolation-test.md](isolation-test.md)).

### `verified`

```json
{ "version": "1.2.3", "status": "verified", "observedOn": "YYYY-MM-DD", "reverify": false }
```

`status` is one of:
- `verified`: installed and used in an EZBox image, with the date and version recorded.
- `observed-in-prior-setup`: worked elsewhere, still to be re-checked here. `ezbx init` marks these as unverified.
- `unverified`: listed only. Not offered by default.

## Rules

1. No secrets in a manifest, ever. Keys stay on the host and the gateway injects them.
2. Pin exact versions and record them in `ezbx.lock`.
3. Do not mark a harness `verified` until it has actually been built and used in an EZBox image.
4. Keep `network.domains` as small as possible. Anything optional goes under `optional`.
5. Autonomy flags are documented per harness and checked against that harness's current docs.
