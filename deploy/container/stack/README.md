# AlixAI container stack (M5-01)

The production packaging: the `alixai` service and its private LocalAI runtime,
hardened, with outbound traffic denied. One set of files for Docker Compose (this directory) and one for rootless
Podman under Quadlet/systemd ([`../quadlet`](../quadlet)); both describe the same topology.

| Path | What it is |
| --- | --- |
| [`../alixai/Dockerfile`](../alixai/Dockerfile) | Multi-stage build. Static binary (`CGO_ENABLED=0`, `-trimpath`), distroless `static` runtime, user 65532, nothing to exec into. Both base images are pinned by digest. |
| [`../alixai/netctl`](../alixai/netctl) | `alixai-netctl`: the loopback health probe and the fixed-destination TCP forwarder the stack needs in a distroless image. About 300 lines, tested. |
| `compose.yaml` | The stack. Profile overrides `compose.lite.yaml`, `compose.balanced.yaml`, `compose.4g.yaml`. |
| `compose.external.yaml` | **Opt-in**: outbound traffic for External provider mode. See below. |
| `compose.noexternal.yaml` | The image variant with no external-provider code (`-tags noexternal`). |
| `config/alixai.yaml` | AlixAI's configuration, mounted read-only. |

## Topology

```text
host 127.0.0.1:7470
        |
     [ relay ]----- edge network (the only route out of the stack; the relay dials the anchor and nothing else)
        |
   front network (internal)
        |
 +-- [ anchor ] ------------------------------------+      one network namespace
 |   [ alixai ]  listens 127.0.0.1:7470             |      (network_mode: service:anchor)
 |   anchor forwards 127.0.0.1:8080 -> localai:8080 |
 +--------------------------------------------------+
        |
   backend network (internal)
        |
    [ localai ]   no ports, never published, no route out
```

- **Two OS identities.** AlixAI runs as 65532, LocalAI as 10001, the forwarders as 65534. Every container has a
  read-only root filesystem, `cap_drop: [ALL]` with no `cap_add`, `no-new-privileges`, a PID limit and a log size cap.
- **LocalAI is reachable only by AlixAI.** It has no `ports`, sits on an internal network, and the only path to it is the
  anchor's forwarder inside AlixAI's own namespace. The relay is on a different network and cannot reach it.
- **Outbound is denied.** Both networks the services share are `internal: true`: Docker gives them no route out.
  LocalAI cannot fetch anything at start, which is why models and backends are provisioned first (below).
- **Start order is health-gated, with no init container:** `localai` healthy, then `anchor` (its health check goes through
  the forwarder to LocalAI's `/readyz`), then `alixai` (`/healthz`), then `relay`. The host port opens only when AlixAI
  answers. Every `depends_on` is `service_healthy`.
- **Data is in bind mounts under `./data`**, never named or anonymous volumes.

Why an anchor and a relay: AlixAI refuses a plaintext LocalAI endpoint that is not loopback (SEC-065) and a listener
beyond loopback without TLS (SEC-093), so it must dial `127.0.0.1:8080` and listen on `127.0.0.1:7470`, and Docker cannot
publish a port of an internal network. The anchor owns the namespace AlixAI runs in and carries the two loopback paths;
the relay carries the host port to the anchor. **The two hops between them are plaintext, over bridge networks that no
other container joins and that have no route out.** The host side is `127.0.0.1` only. AlixAI sees the relay as the peer
address of every request, not the real client.

Two consequences to know. The anchor listens on every interface of its namespace, so it also listens on the `backend`
network: LocalAI can open a connection to AlixAI's API (every route but `/healthz` and `/readyz` still needs its
credentials), while the reverse path, AlixAI to LocalAI, is the intended one. And in Compose only, a restart of the anchor
container leaves `alixai` in the namespace it had: its own `/healthz` keeps passing while the relay path is dead until
`alixai` is restarted too (`docker compose restart anchor alixai relay`). Quadlet does not have this: the pod's own
infrastructure container owns the namespace.

## Prepare the host

```bash
cd deploy/container/stack
cp .env.example .env                       # set LOCALAI_API_KEY (openssl rand -hex 32)
mkdir -p data/alixai data/localai/models data/localai/backends data/localai/state
chmod 700 data/alixai
# Linux with rootful Docker: the data directories belong to the container users. Docker Desktop ignores owners.
sudo chown 65532:65532 data/alixai
sudo chown -R 10001:10001 data/localai
```

Then provision the **backend** and the **model files** into `data/localai/backends` and `data/localai/models` before the
first start. The stack has no route out, so LocalAI cannot download them, and model downloads happen only through an
operator-approved step (16.1). Use the host-side steps of
[`../localai/README.md`](../localai/README.md#backend-and-model-pull-from-the-host), pointing them at this `./data/localai`.
LocalAI re-checks every sha256 pin at start. The LocalAI configuration (`models.yaml`, `backend-gallery.yaml`) is read
from `../localai/config` by default; set `ALIXAI_LOCALAI_CONFIG` in `.env` to a directory with your approved definitions.

`config/alixai.yaml` ships with `inference.models: []`, which keeps the inference path off: the service starts, `/healthz`
answers and `/readyz` says why the model path is unavailable. Set it to the registry ids you provisioned once their entries
are activatable; AlixAI verifies each file against its registry pin before it serves a request (SEC-063).

```bash
docker compose -f compose.yaml -f compose.lite.yaml up -d --build      # or compose.balanced.yaml, compose.4g.yaml
curl -fsS http://127.0.0.1:7470/healthz
curl -sS  http://127.0.0.1:7470/readyz
docker compose -f compose.yaml -f compose.lite.yaml down                # keeps ./data
```

`podman compose` takes the same files. Rootless Podman in production is the Quadlet path below.

### Profiles

The profile files cap the LocalAI container exactly as `../localai/compose.<profile>.yaml` does (`mem_limit`,
`memswap_limit` equal to it so there is no swap, `cpus`, `LOCALAI_THREADS`, resident backends) and set `ALIXAI_PROFILE`.
The AlixAI container has its own 512 MB cap; the core measured 96 to 160 MB (`docs/operations/resource-budgets.md`), and
the two forwarders 64 MB each. Anything measured under these files is labelled **emulated**, never certified. The 4 GB
profile is an aspiration until the full workload passes its gates; it runs the lite context budgets.

## Reaching the API

The API is on `127.0.0.1:${ALIXAI_PORT:-7470}` of the host. Exposure beyond the machine is a reverse proxy on the host
that terminates TLS and forwards to that port; it is the operator's, with its own service identity. Do not change the
`ports` entry to another address: `TestPortsLoopbackOnly` and the repository's compose gate refuse it.

## External provider mode (opt-in)

The default stack **denies all outbound traffic**. `compose.external.yaml` is the opt-in for External provider mode
(milestone X):

```bash
docker compose -f compose.yaml -f compose.lite.yaml -f compose.external.yaml up -d
```

It joins the anchor, whose namespace AlixAI runs in, to a bridge network with a route out. LocalAI stays on its internal
network and gets none. The Quadlet equivalent is [`../quadlet/external`](../quadlet/external).

- **It makes the deployment network-dependent and subject to provider terms**: the providers' terms of service, data
  handling and availability. Prompts that the data-egress policy allows leave the machine. Local mode and the default stack are unchanged.
- **Compose and Podman cannot filter by host name.** The file opens the network layer for the AlixAI process and nothing
  narrower. "Only the configured provider hosts" is enforced by AlixAI itself: every outbound connection goes through
  `internal/egress`, which dials only the exact host and port entries of the configured allowlist and refuses everything
  else, including names that resolve to private or link-local addresses. For the same rule at the network layer, add host
  firewall rules for the provider addresses; a proxy sidecar would not help, because `internal/egress` ignores proxy
  environment variables on purpose.
- A deployment that must never contain external code uses the `noexternal` variant instead.

## The `noexternal` image variant

```bash
docker compose -f compose.yaml -f compose.lite.yaml -f compose.noexternal.yaml up -d --build
docker build -f deploy/container/alixai/Dockerfile --build-arg GO_TAGS=noexternal -t alixai:noexternal .
```

Built with `-tags noexternal`: the external-provider registration is not compiled in, so nothing of milestone X is linked
and External mode cannot be enabled whatever the configuration says. Do not combine it with `compose.external.yaml`.

## Rootless Podman with Quadlet

Needs Podman 5.2 or later (`.pod` and `.build` units) and systemd user services. **Written to
`podman-systemd.unit(5)` and checked statically; not run on a Linux host** (see Verification).

```bash
cd deploy/container/quadlet
Q=~/.config/containers/systemd; mkdir -p "$Q" ~/.config/alixai/localai ~/.local/share/alixai/{alixai,localai/{models,backends,state}}
cp *.container *.pod *.network *.build "$Q"/                       # edit the two paths in alixai*.build to your checkout
cp -r profiles/lite/* "$Q"/                                        # or balanced, 4g
install -m 0644 localai.env.example ~/.config/alixai/localai.env
install -m 0600 alixai.env.example  ~/.config/alixai/alixai.env     # set LOCALAI_API_KEY
install -m 0644 ../stack/config/alixai.yaml ~/.config/alixai/alixai.yaml
install -m 0644 ../localai/config/*.yaml ~/.config/alixai/localai/
install -m 0644 ../localai/entrypoint.sh ~/.config/alixai/localai-entrypoint.sh
# rootless: container uids map to different subuids, so the data directories are chowned inside the user namespace
podman unshare chown 65532:65532 ~/.local/share/alixai/alixai; chmod 700 ~/.local/share/alixai/alixai
podman unshare chown -R 10001:10001 ~/.local/share/alixai/localai
systemctl --user daemon-reload
systemctl --user start alixai-relay.service                         # pulls the whole chain in order
```

Provision the backend and models into `~/.local/share/alixai/localai` as above. For `systemctl --user` to start at boot
and to enforce memory limits, run `loginctl enable-linger $USER` and delegate the `memory` and `cpu` cgroup controllers to
the user. On SELinux hosts the units already carry `:z` and `:Z` labels. External mode: copy `external/alixai-egress.network`
and `external/alixai.pod.d` into `$Q`. The `noexternal` variant: copy `noexternal/alixai.build.d` into `$Q`.

`Notify=healthy` makes systemd treat a unit as started only when its health check passes, and the `Requires=`/`After=`
chain is the same one as in Compose: `alixai-localai`, `alixai-anchor`, `alixai`, `alixai-relay`.

## What is enforced, and by what

| Rule | Where | Checked by |
| --- | --- | --- |
| Separate non-root identities, read-only root, no capability, no-new-privileges | `compose.yaml`, `quadlet/*.container` | `TestComposeHardening`, `TestQuadletHardening` |
| No `ports` on LocalAI, one loopback-only published port | `compose.yaml` relay, `alixai-relay.container` | `TestPortsLoopbackOnly` |
| No init or one-shot container, every start order gated on a health check | both | `TestNoInitContainers`, `TestQuadletStartOrderIsHealthGated` |
| LocalAI pins, off switches and profile caps equal M0-08's | both | `TestStackMatchesLocalAIDeployment` |
| Outbound only through the opt-in, and only to the AlixAI namespace | `compose.external.yaml`, `quadlet/external` | `TestExternalOverrideIsOptIn` |
| Pinned bases, static binary, distroless non-root, no shell, no `VOLUME`, `noexternal` switch | `Dockerfile` | `TestDockerfileHardening`; `TestDockerBuildSmoke` (`-tags live`) |
| The repository-wide compose gate (loopback ports, digests, LocalAI off flags) | every compose file | the repository-wide invariant tests, run by `go test ./...` |

`go test ./tests/deploy/...` runs the static checks; `go test -tags live ./tests/deploy/ -run TestDockerBuildSmoke` builds
the three images and checks user, volumes, shell and read-only start.

## Verification state

Run on Docker Desktop (Windows, **emulated**, not a Linux host): the three images build; `docker compose config` is
valid for the base file and every profile; the four containers start in the gated order and report healthy with a stand-in
for LocalAI; `GET /healthz` through the published loopback port returns 200 and `/readyz` reports the inference path off;
LocalAI is not published; a wget to an outside address fails from the AlixAI namespace and from LocalAI, and succeeds from
the AlixAI namespace only with `compose.external.yaml`; the containers run with the users, read-only root and dropped
capabilities stated above.

**Not run:** the real LocalAI image inside the stack (so no chat completion through it; no model is activatable yet),
rootful Docker and rootless Podman on Linux, `podman compose`, every Quadlet unit, SELinux labels, ARM64, `-tags noexternal`
with milestone X code (the registration files do not exist yet; the build tag is wired and the image builds).
