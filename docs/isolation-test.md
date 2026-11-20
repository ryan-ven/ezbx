# Isolation test

The central safety claim of EZBox is that the box can reach the gateway's model and proxy ports and nothing else. This page records how that is tested and what the results were.

## Setup

Everything is built from this repository and labelled `ezbx.managed=true`.

| Part | What it is |
|---|---|
| `ezbx-gateway:dev` | [docker/gateway.Dockerfile](../docker/gateway.Dockerfile): the `ezbx` binary alone in a `scratch` image (about 11 MB), run as user 65532 with a read-only root filesystem, every capability dropped and `no-new-privileges`. |
| `ezbx-probe:dev` | [docker/probe.Dockerfile](../docker/probe.Dockerfile): [tools/isoprobe](../tools/isoprobe/main.go), which tries each target and prints PASS or FAIL. It runs with the same restrictions the box will have. |
| fake engine | [tools/fakeengine](../tools/fakeengine/main.go), listening on the host's loopback address only, like a real engine. No real model is involved. |
| inside network | `docker network create --internal -o com.docker.network.bridge.gateway_mode_ipv4=isolated ...` |
| outside network | an ordinary bridge network, not Docker's default one. |

The gateway is attached to both networks and forwards to `http://host.docker.internal:18080`. The probe is attached to the inside network only.

## What the probe tries from the inside network

- The model route through the gateway: **must work**.
- Every TCP port on the gateway: only the model port may be open.
- Public addresses by IP (HTTPS) and public names (DNS through Docker's resolver).
- Public DNS servers, the LAN router and Docker Desktop's DNS, asked directly.
- Docker Desktop's own addresses (`host.docker.internal`, `gateway.docker.internal`, `kubernetes.docker.internal`, and the addresses they resolve to on the outside network), including the port the engine listens on.
- The host's LAN address and its other network adapters.
- Other containers, on the outside network and on Docker's default bridge, by name and by address.
- The gateway's own outside-network address, on every port. Linux accepts a packet for any of its own addresses on any interface, so this checks that the box has no route there.
- The bridge addresses of the outside network, the default bridge and the inside network.
- The cloud metadata address `169.254.169.254`.

## Results (2026-11-20, Docker Desktop on Windows 11, Engine 29.9.3)

**With a plain `--internal` network, one gap was found.** The inside network's bridge still has an address, and that address belongs to Docker Desktop's Linux VM. From the box, that address answered on TCP port 111 (the RPC portmapper) and on one high port. Anything listening on all addresses inside the VM was reachable from the box. Everything else was blocked.

**With `gateway_mode_ipv4=isolated` added, every check passed.** The bridge has no address at all, so there is nothing of the VM's to connect to. The model route worked, the gateway showed only its model port, and every other target failed with "network is unreachable". Names outside the network didn't resolve.

Decided from this: the inside network is always created with `--internal` **and** `gateway_mode_ipv4=isolated`, with IPv6 off. That option needs Docker Engine 28 or later, and `ezbx doctor` must check for it.

Also confirmed on the outside network: a container there reaches an engine bound to the host's loopback address through `host.docker.internal`.

### The box image (same day)

The first box image ([docker/box.Dockerfile](../docker/box.Dockerfile): Node 24 LTS, git and Pi 1.5.0) was run on the isolated inside network as user `agent`, with every capability dropped, `no-new-privileges`, and memory and process limits.

- The probe, run inside the box image itself, passed every check above, plus `registry.npmjs.org` and `github.com` not resolving.
- Inside the box: no capabilities at all, no file with a setuid or setgid bit, no `sudo`, and the Pi install belongs to root, so the agent can run it but not change it.
- `pi -p` with a config written by hand from the Pi manifest's template sent its request through the gateway to the fake engine on the host, and printed the reply. The gateway log shows the request. Pi needed nothing else from the network. It wrote only two empty files to its config folder and nothing to the workspace.

Not covered: Pi with a real model, tool calls, and the interactive interface.

### The egress proxy and the admin API (same day)

The gateway ran with the proxy on port 3128 (starting in `off`) and the admin API on its own loopback, with the token file mounted read-only. Docker reported the gateway healthy through `ezbx gateway health`. Every change below was made from the host with `docker exec ... ezbx gateway admin`. Tests ran from the box image with the usual proxy variables set.

- **Admin API out of reach.** A full port scan of the gateway from the box found only 8000 and 3128. Port 8099 was closed as `gateway:8099`, and through the proxy as `127.0.0.1:8099`, `localhost:8099`, `[::ffff:127.0.0.1]:8099` and `0.0.0.0:8099`.
- **`off`:** every proxy request got 403, including `example.com`, `github.com` and `1.1.1.1`.
- **`allow` with `registry.npmjs.org`:** `npm view left-pad version` worked through the proxy. `git ls-remote https://github.com/...` was refused with 403 (`not_on_allowlist`), then worked after `allow github.com`.
- **`on`:** tunnels to `example.com:443` and `1.1.1.1:443` opened. Refused with 403: `host.docker.internal`, `gateway.docker.internal`, Docker Desktop's host and DNS addresses, the bridge addresses, `169.254.169.254`, `10.0.0.1`, `[::1]`, and ports 22 and 8080. Also refused: public names that resolve to local addresses (`localtest.me` to 127.0.0.1, and `169.254.169.254.nip.io` and `10.0.0.1.nip.io`). The log gave `local_address` as the reason, so the check after the lookup caught them.
- **Tightening:** with a tunnel to `example.com` open, switching from `on` to `allow` (npm registry only) closed it at once. The admin reply said one tunnel was closed, and its log line says `network_tightened`.
- The admin token never appeared in the gateway log.

### With a real engine (same day)

Strata ran on the host, bound to its loopback address, with one model loaded (a reasoning model with a 262,144-token context and text and image input). The gateway forwarded to it through `host.docker.internal`.

- `ezbx engine detect` on the host found it on its custom port through Strata's own status command, with the model's context size, input types and loaded state.
- **Found and fixed:** Strata refused every request from the gateway with 403, because of its DNS rebinding protection: the Host header was `host.docker.internal:PORT`, which isn't a local name. Readiness hid this by saying "no models". The gateway now sends `Host: localhost:PORT` when the upstream is Docker Desktop's host alias, and readiness reports the engine's status code (see [gateway.md](gateway.md), section 4).
- After the fix, from the box: the model list, a chat reply, and a streamed reply that arrived in 10 chunks (first after 7 ms). A stream cancelled by the box was logged as 499 `client_gone`, and the gateway kept working. `/v1/../props`, `/v1/%2e%2e/props`, `/props`, `/readyz` and `DELETE` were refused.
- **Pi with the real model**, on the isolated network: it answered a plain prompt, used its read tool to find a word in a workspace file, and used its write tool to create a file with exact content, which appeared in the workspace folder on the host. Each took 3 to 7 seconds. Every request went through the gateway.
- Then again with a config produced by `ezbx harness render pi --reasoning`, with no hand edits: Pi read a word from a workspace file and wrote it into a new file, in 6 seconds.
- The model is a reasoning model: with a 16-token limit, the reply stopped inside its reasoning with no answer. Pi's config needed `reasoning: true` and room for the answer (8,192 tokens). Engines don't report this, so the config generator must take it from a default or the user.

Not covered: Pi's interactive interface.

### `ezbx doctor` (same day)

On the setup above (isolated inside network, locked-down gateway in front of Strata, a running box container), `ezbx doctor` passed all 21 checks in about 4 seconds.

Then a deliberately broken box: the inside network made with `--internal` alone; a gateway running as root, writable, with its capabilities and a port published to every address; and a box running as root, with its capabilities, also attached to Docker's default bridge, with a file mounted at `/var/run/docker.sock` (an empty file, not the real socket). The doctor failed exactly those seven things, each with a fix. From inside, its port scan of the network's bridge address found the Docker VM's ports open, as the first test above did by hand. The broken box was removed straight after, because its published port made the engine reachable from the network.

The unit tests feed it recorded `docker inspect` output and check that 26 separate faults are each caught by the right check.

## Limits of this result

- On the test machine, public DNS servers didn't answer even from the outside network, so the "public DNS blocked" checks prove less than they appear to. The checks against Docker Desktop's own DNS server, which did answer from the outside, are the meaningful ones.
- Only TCP and DNS over UDP were tested. Other UDP and ICMP were not.
- The proxy test used live public sites and public DNS. The address checks themselves are also covered by unit tests that need no network.
- One machine, one Docker Desktop version. `ezbx doctor` now repeats the main checks, so run it after every Docker Desktop upgrade.
