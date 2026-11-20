# Engine adapters (schema 1, draft)

An engine is the program that runs the model on your machine. EZBox supports several, and each has a small adapter in `providers/<id>/adapter.json`. Adapters are plain data, so adding an engine doesn't need a code change unless it behaves in a new way.

All engines are expected to offer an OpenAI-style HTTP API. That is what lets the gateway treat them alike.

## What an adapter says

| Section | Meaning |
|---|---|
| `detect` | How to find the engine: hint ports, an optional command that lists running servers, and response probes. |
| `openai` | Paths for the model list and chat. |
| `models` | Where to read each model fact from the engine's replies (id, context size, image input, loaded state). A `null` means the engine doesn't report it, so a configured default is used. |
| `auth` | Whether the engine wants a key. The key's value is never stored here. |
| `capabilities` | What the engine can do, as far as we know. |
| `control` | Optional commands or calls to start, stop, check or unload. `null` means EZBox detects the engine but doesn't control it. |
| `verified` | What has actually been checked, and when. |

## Detection

Ports are hints, never proof. Different versions and user setups use different ports, and one source says llama.cpp's default has changed. Detection therefore works like this:

1. If an adapter has a `cli` status command and that program is installed, ask it for running servers. This finds engines on custom ports.
2. Probe each adapter's hint ports on the loopback address only, with a very short timeout. Probes are plain GET requests with no key and no data about you.
3. Fingerprint each answer using the adapter's `probes`. An engine counts as detected only if every required probe answers with what it expects (see below).
4. When more than one adapter matches the same server (some engines share routes), the more specific one wins: the one whose probes checked the most fields. Strata and llama.cpp both expose a health route, for example, and Strata's includes extra fields.
5. Report what was found. If several engines are running, `ezbx init` asks which to use.

A server that offers an OpenAI-style model list but matches no specific adapter is reported as an **OpenAI-compatible server** (the `openai` adapter). It works with the gateway, but EZBox doesn't claim to know which engine it is. That adapter adds no ports of its own, and any specific adapter that matches wins over it.

If detection fails, you can give the address yourself in `ezbx.local.toml`.

### Probes

Each probe is a GET to one path. Its `expect` says what the JSON reply must contain:

- `hasFields`: paths that must lead to real (non-null) values. A path with `[]`, such as `data[].id`, needs a non-empty list.
- `hasLists`: plain paths, such as `data`, that must be lists. An empty list counts, so an engine with no models loaded is still found.

A probe must expect at least one thing. `"optional": true` makes a probe add to the score when it matches without being needed. Two probes may use the same path; it is fetched once.

### Model facts in the adapter

`models.id`, `contextWindow` and `input` point into the model list, for example `data[].meta.n_ctx`. `models.loaded` has one of two forms:

| Form | Meaning |
|---|---|
| `data[].status.value == loaded` | A field in the model list equals a value. |
| `/api/ps models[].name` | The model is loaded if another route lists its id there. If that route doesn't answer, loaded stays unknown. |

Anything else is refused when adapters load, so a fact is never silently ignored.

## Model facts

The gateway and CLI read the engine's model list and fill in what the engine reports.

- **Strata** reports context size and image input in its model list, and a loaded state.
- **Ollama** reports which models are loaded through `/api/ps`.
- **LM Studio, Ollama and llama.cpp** report little else in the standard list. LM Studio's native list (`/api/v1/models`) has `max_context_length` and `loaded_instances`, which are not read yet. EZBox uses configured defaults (and can later ask each engine's native API) and lets you override them in `ezbx.local.toml`.

These facts feed the harness config generator (see `harness-manifest.md`).

## Keys

If an engine requires a key, you name an environment variable in your local config (`key_env`). The gateway reads it when it starts and adds the key to requests. The key never appears in a repo file, an image or a log.

## Reaching the engine

Engines normally listen on the loopback address only, which is the safe default. The gateway reaches them through the host alias on Docker Desktop, and presents itself as `localhost`, because engines with DNS rebinding protection (Strata, for one) refuse other host names (see [gateway.md](gateway.md), section 4). Binding an engine to all interfaces just to make this work isn't recommended, because some engines have no authentication (LM Studio's docs warn about this). Setups other than Docker Desktop are covered by design decision 4.

## Status of each adapter

| Engine | Status | Notes |
|---|---|---|
| Strata | **probed-live** (2026-11-20) | Detection fields and control commands were read from a running install. Context size and image input come from its model list. |
| LM Studio | docs-only | Recognised by its native `GET /api/v1/models` (LM Studio 0.4.0 and later, from its docs read 2026-11-20). Older versions, or one with its API token on, show up as a generic OpenAI-compatible server. The lms commands need confirming on a real install. |
| Ollama | docs-only | The OpenAI-compatible base path is from official docs. The port, the running-models route and the unload call are from third-party guides. |
| llama.cpp | docs-only | Port is disputed between sources. Detection only. Recognised by `/health` with a `status` field, which is a weak sign: another server with such a route would be labelled llama.cpp. |
| OpenAI-compatible | generic | The fallback for any server with an OpenAI-style model list. |

Do not change a status to `verified` until the adapter has been exercised against a real install and the date recorded.

## Sources

- [Ollama OpenAI compatibility](https://docs.ollama.com/api/openai-compatibility) (official)
- [LM Studio local server](https://lmstudio.ai/docs/local-server) (official)
- [LM Studio REST API: list models](https://lmstudio.ai/docs/developer/rest/list) and [REST API v0](https://lmstudio.ai/docs/developer/rest/endpoints) (official)
- [llama.cpp server README](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md) (official, but not read in full here)
- Third-party guides were used for Ollama's `/api/ps` and `keep_alive`, and for the llama.cpp port disagreement. They are flagged in the adapters.

## Open questions

1. Whether to read richer model facts from each engine's native API in version 0.1 or leave them as defaults.
2. How to tell the user clearly when two engines are running and which one the gateway is using.
