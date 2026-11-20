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

## Limits of this result

- On the test machine, public DNS servers didn't answer even from the outside network, so the "public DNS blocked" checks prove less than they appear to. The checks against Docker Desktop's own DNS server, which did answer from the outside, are the meaningful ones.
- Only TCP and DNS over UDP were tested. Other UDP and ICMP were not.
- The proxy test used live public sites and public DNS. The address checks themselves are also covered by unit tests that need no network.
- One machine, one Docker Desktop version. The test should be re-run on upgrades and become part of `ezbx doctor`.
