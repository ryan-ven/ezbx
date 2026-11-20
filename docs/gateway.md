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

The admin listener is published to the host loopback only and needs a token from the host state folder. The box has no route to the host, so it cannot reach it. Ports are examples and will be configurable.

## 3. The model route

### Forwarding

- Paths are passed straight through: `/v1/models`, `/v1/chat/completions`, `/v1/embeddings` and any other `/v1/...` path. The gateway doesn't interpret request bodies except to log the model name.
- The upstream address is fixed by configuration, so the route can't be turned into a general-purpose relay.
- Request size is capped to a generous default.

### Streaming

Responses that stream (server-sent events) are forwarded as they arrive, with no buffering. If the box closes the connection, the gateway cancels the upstream request so the engine stops generating and frees the GPU. There is no overall timeout on a stream, only an idle timeout.

### Authentication

- **Box to gateway:** a placeholder key. It is not a real credential and exists because some harnesses insist on one.
- **Gateway to engine:** if the engine needs a key, the gateway adds it. The key is read at start from the host's secret store or an env file and is never written into an image, a config the box can read, or a log.

### Model list

`GET /v1/models` returns what the engine reports, as is. The CLI reads the same list to generate harness configs (see `harness-manifest.md`). A fixed alias such as `default` is a possible later addition, so configs wouldn't need regenerating when the model changes.

### Response headers

Every model response carries:

- `X-Ezbox-Engine`: the engine id (for example `strata`).
- `X-Ezbox-Model`: the model name from the request.

These let `ezbox doctor` prove which engine answered (open question 4 in `design.md`).

## 4. Switching engines

1. `ezbox engine use <id>` updates the config and calls the admin API.
2. The gateway swaps its upstream. Requests already in flight finish on the old engine, and new requests go to the new one.
3. If the model list changed, the CLI regenerates harness configs (`ezbox harness sync`).

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
- **Blocked requests are logged** with the host and the reason, so `ezbox net allow <domain>` is easy to use.
- **Tools that ignore proxy settings** simply fail, because the box has no other route. That is the desired result.

The mode is changed through the admin API and takes effect for new connections. Open tunnels are closed when the mode tightens.

## 6. Logging

One JSON object per line, written to the state folder (default location is open question 3).

| Field | Meaning |
|---|---|
| `time`, `id` | Timestamp and request id. |
| `route` | `model` or `proxy`. |
| `method`, `path` or `host` | What was requested. |
| `model` | Model name from the request, when present. |
| `status`, `blocked`, `reason` | Outcome, and why if blocked. |
| `ms`, `ttfb_ms` | Total time and time to the first byte. |
| `tokens_in`, `tokens_out` | When the response reports usage. **Verify** how each engine reports usage in streams. |

Prompts and responses are **not** logged unless you turn that on. Logs rotate by size.

## 7. Health and status

| Endpoint | Meaning |
|---|---|
| `/healthz` | The gateway process is alive. |
| `/readyz` | The upstream engine answered and listed at least one model. |
| admin `/admin/status` | Engine, upstream address, model count, network mode and counters. |

### What `ezbox doctor` checks

1. Gateway running and ready.
2. From inside the box, the model list comes back through the gateway.
3. A tiny completion succeeds and the headers name the expected engine and model.
4. The box cannot reach the internet when the mode is `off`, and cannot reach the host or LAN in any mode.

## 8. Failures

Errors use the OpenAI-style error body so harnesses display them sensibly.

| Situation | Response |
|---|---|
| Engine not reachable | 502, with a message telling you to run `ezbox doctor`. |
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

## 12. Milestones

- **0.1:** model route, streaming, key injection, response headers, `off` and `on` modes, logging, health, admin API.
- **0.2:** `allow` mode, blocked-request workflow, `doctor` end-to-end checks.
- **Later:** the `default` alias, usage metering, several engines at once.

## 13. Open questions

1. How each engine reports token usage in streamed responses.
2. Whether the gateway should enforce one request at a time per engine, or leave queuing to the engine.
3. Default location of the state folder and logs (shared with `design.md`).
