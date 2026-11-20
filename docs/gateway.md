# Gateway design (draft 0.1)

The gateway is the only door between the box and everything else. This page describes what it does, how it is controlled, and how we will test it. Items marked **verify** depend on engine behavior I still need to confirm.

## 1. Where it sits

```text
 box (inside network) ──► gateway ──► engine on the host   (model route)
                              └─────► the internet           (egress proxy, by network mode)

 host CLI ──► gateway admin API (host loopback only)
```

The gateway runs as its own container, as an unprivileged user, with all capabilities dropped and a read-only root filesystem. It mounts only a read-only config file and a writable state folder for logs.

## 2. Listeners

| Listener | Reachable from | Purpose |
|---|---|---|
| **Model** (`:8000`) | the box | OpenAI-compatible API, forwarded to the selected engine. |
| **Proxy** (`:3128`) | the box | HTTP forward proxy for internet access, governed by the network mode. |
| **Admin** (`127.0.0.1:PORT` on the host) | the host only | Control and status for the CLI. |

The admin listener is published to the host loopback only and needs a token from the host state folder. Publishing to loopback is not enough on its own, though: the box shares a network with the gateway container, so it can reach any port the gateway listens on there, whatever is published to the host. The admin listener must therefore not listen on the box's network at all (for example, bind only to the gateway's outside address, or use a separate small container), and the token stays required as a second layer. How exactly is not decided yet, and the end-to-end test must show the box can't reach it. Ports are examples and will be configurable.

## 3. The model route

### Forwarding

- Paths are passed straight through: `/v1/models`, `/v1/chat/completions`, `/v1/embeddings` and any other `/v1/...` path. The gateway doesn't interpret request bodies except to log the model name.
- Paths that an engine might resolve to somewhere outside `/v1/` are refused with 400: `.` or `..` segments (also when encoded, such as `%2e%2e`), `//`, backslashes and control characters. An encoded slash inside a name, such as `/v1/models/org%2Fmodel`, is allowed.
- Only `GET`, `HEAD` and `POST` are forwarded (others get 405), and protocol upgrades such as WebSocket are refused.
- The upstream address is fixed by configuration, so the route can't be turned into a general-purpose relay.
- Request size is capped (32 MiB by default, enough for several images). All bodies held at once share one memory budget (256 MiB by default). A request that would go over it gets a 503 "busy" reply with `Retry-After`, rather than waiting. A client has 2 minutes to send its body.

### Streaming

Responses that stream (server-sent events) are forwarded as they arrive, with no buffering. If the box closes the connection, the gateway cancels the upstream request so the engine stops generating and frees the GPU. There is no overall timeout on a stream. There are two generous limits instead:

- **Waiting for the reply to start:** 30 minutes by default, then 504. A reply that is not streamed only starts once the whole answer is ready, so this has to be long.
- **Idle:** if the engine sends nothing for 10 minutes in the middle of a reply, the gateway ends it and cancels the engine request.

### Authentication

- **Box to gateway:** a placeholder key. It is not a real credential and exists because some harnesses insist on one.
- **Gateway to engine:** if the engine needs a key, the gateway adds it. Today the key is read at start from an environment variable whose name is given with `--key-env` (whether to also use the operating system's credential store is open question 1 in `init-flow.md`). It is never written into an image, a config the box can read, or a log.

### Model list

`GET /v1/models` returns what the engine reports, as is. The CLI reads the same list to generate harness configs (see `harness-manifest.md`). A fixed alias such as `default` is a possible later addition, so configs wouldn't need regenerating when the model changes.

### Response headers

Every reply that comes from the engine carries:

- `X-Ezbx-Engine`: the engine id (for example `strata`).
- `X-Ezbx-Request-Id`: the id of the matching log line.
- `X-Ezbx-Model`: the model name from the request, only when the request named one.

Any `X-Ezbx-*` header the engine sends itself is removed first, so only the gateway can set them. Errors the gateway writes itself (400, 405, 413, 502, 503, 504) don't carry these headers.

These let `ezbx doctor` prove which engine answered (open question 4 in `design.md`).

## 4. Switching engines

1. `ezbx engine use <id>` updates the config and calls the admin API.
2. The gateway swaps its upstream. Requests already in flight finish on the old engine, and new requests go to the new one.
3. If the model list changed, the CLI regenerates harness configs (`ezbx harness sync`).

Reaching the host: on Docker Desktop the gateway uses the host alias. That route worked for an engine bound to loopback in the predecessor setup. Other setups come later (decision 4).

## 5. The egress proxy

The proxy is how the box reaches the internet, if at all.

| Mode | Behavior |
|---|---|
| `off` | Every proxy request is refused. Only the model route works. |
| `allow` | Requests are allowed only to hosts on the allowlist. |
| `on` | Requests to the public internet are allowed. |

Rules for every mode:

- **Hostnames, not content.** HTTPS goes through a plain tunnel and is never decrypted, so filtering is by domain. There is no path-level filtering.
- **Local networks stay blocked**, even in `on`. The proxy refuses private ranges, loopback, link-local and cloud metadata addresses, plus the host alias, unless the config explicitly allows one. Without this, the box could reach other services on your PC or LAN through the gateway.
- **Resolve once, then connect.** The gateway resolves the name itself and checks the resulting address against the blocked ranges before connecting, so a hostname can't point at a private address.
- **Blocked requests are logged** with the host and the reason, so `ezbx net allow <domain>` is easy to use.
- **Tools that ignore proxy settings** simply fail, because the box has no other route. That is the desired result.

The mode is changed through the admin API and takes effect for new connections. Open tunnels are closed when the mode tightens.

## 6. Logging

One JSON object per line, written to the per-user state folder (decided in `init-flow.md`). Today `ezbx gateway serve` writes to standard output or to the file given with `--log`.

| Field | Meaning |
|---|---|
| `time`, `id` | Timestamp and request id. |
| `route` | `model` or `proxy`. |
| `method`, `path` or `host` | What was requested. |
| `model` | Model name from the request, when present. |
| `status`, `blocked`, `reason` | Outcome, and why if blocked. |
| `ms`, `ttfb_ms` | Total time and time to the first byte. |
| `end` | Only when a reply was cut short: `client_gone` (the box left; the status is then 499) or `engine_idle` (the engine went quiet). |
| `tokens_in`, `tokens_out` | Planned. When the response reports usage. **Verify** how each engine reports usage in streams. |

Built today: `time`, `id`, `route`, `method`, `path`, `engine`, `model`, `status`, `ms`, `ttfb_ms` and `end`. The proxy fields and token counts come with the egress proxy and usage metering.

Prompts and responses are **not** logged. A switch to log them, and rotation by size, are planned.

## 7. Health and status

| Endpoint | Meaning |
|---|---|
| `/healthz` | The gateway process is alive. |
| `/readyz` | The upstream engine answered and listed at least one model. Each call asks the engine, so it may move to the admin listener once that exists, leaving only `/healthz` on the box's side. |
| admin `/admin/status` | Engine, upstream address, model count, network mode and counters. |

### What `ezbx doctor` checks

1. Gateway running and ready.
2. From inside the box, the model list comes back through the gateway.
3. A tiny completion succeeds and the headers name the expected engine and model.
4. The box cannot reach the internet when the mode is `off`, and cannot reach the host or LAN in any mode.

## 8. Failures

Errors use the OpenAI-style error body so harnesses display them sensibly.

| Situation | Response |
|---|---|
| Engine not reachable | 502, with a message telling you to run `ezbx doctor`. |
| Engine didn't start answering in time | 504. |
| Too many large requests held at once | 503 with `Retry-After`. |
| Body too large | 413. |
| Path not allowed, or a protocol upgrade | 400. |
| Method other than GET, HEAD or POST | 405. |
| Engine still loading a model | The engine's own error, passed through. |
| Proxy blocked | 403 with the reason in the body, and a log line. |

## 9. Admin API (first set)

| Call | Purpose |
|---|---|
| `GET /admin/status` | Current state. |
| `PUT /admin/engine` | Switch the upstream engine. |
| `PUT /admin/network` | Set the mode, or add an allowlisted domain. |
| `POST /admin/engine/unload` | Ask the engine to unload its model, if the adapter supports it ("gaming mode"). |

## 10. Implementation notes

- Go standard library: `net/http` with `httputil.ReverseProxy` (flush immediately for streams) for the model route, and a small CONNECT handler for the proxy.
- One binary shared with the CLI.
- Image: minimal, no shell, runs as a non-root user.

## 11. Test plan

- A fake engine server that serves normal responses, streams and errors, and can drop connections.
- Tests for streaming without buffering, upstream cancellation on client disconnect, key injection, header stamping and the error bodies.
- Table tests for the allowlist and the blocked ranges, including hostnames that resolve to private addresses.
- An end-to-end run with Docker: the box can reach the model route and nothing else when the mode is `off`.

## 11a. Implementation status

Built and tested (`internal/gateway`, run with `ezbx gateway serve`):

- Model route for `/v1/...` only, to one fixed upstream, with address checks.
- Path checks (no `.`/`..` segments, `//`, backslashes or control characters), only `GET`/`HEAD`/`POST`, and no protocol upgrades.
- Streaming without buffering, and cancellation of the engine request when the client leaves.
- Limits for waiting on the engine to start a reply and for an engine that goes quiet mid-reply.
- Key injection (the box's placeholder is dropped), and no `X-Forwarded-For` or `Forwarded`.
- `X-Ezbx-Engine` and `X-Ezbx-Request-Id` on every reply from the engine, `X-Ezbx-Model` when the request named a model, and engine-set `X-Ezbx-*` headers removed.
- `/healthz` and `/readyz`, OpenAI-style errors, a request size limit, a shared memory budget for request bodies, and JSON logging with no prompts, replies or keys (499 and `end` when a reply is cut short).
- Switching the upstream at run time in code (`SetUpstream`).

Checked against a real engine (Strata): readiness, model list, a normal chat reply and a streamed reply that arrived chunk by chunk. That check was done before the path checks, limits and memory budget were added.

Not built yet: the egress proxy and network modes, the admin API (so `SetUpstream` has no outside control yet), token usage in the log, and the Docker packaging.

## 12. Milestones

- **0.1:** model route, streaming, key injection, response headers, `off` and `on` modes, logging, health, admin API.
- **0.2:** `allow` mode, blocked-request workflow, `doctor` end-to-end checks.
- **Later:** the `default` alias, usage metering, several engines at once.

## 13. Open questions

1. How each engine reports token usage in streamed responses.
2. Whether the gateway should enforce one request at a time per engine, or leave queuing to the engine.
3. ~~Default location of the state folder and logs.~~ Decided in `init-flow.md`: a per-user state folder, never inside the workspace and never mounted into the box.
