# CLM sidecar hardening (vLLM v0.31.0 CPU image, CLM service 0.1.0)

The sidecar is an **optional** scorer for the decision layer (ADR 0029, ADR 0030 provisional preference): **bounded**
(at most 32 authorised candidates and 2048 state tokens per call), **non-generative** (it ranks supplied candidates and
writes no text) and **non-authoritative** (its result is advisory; the controller re-resolves every selected id). This
page lists what the deployment disables, what it still exposes, and the three outbound calls. Everything here is
configuration read on 2026-10-07; nothing was started to produce it, so every runtime statement is a design
intent that the live run (D-08) confirms or corrects.

## 1. Posture

| Property | Setting |
| --- | --- |
| Network exposure | only the CLM service publishes a port, `127.0.0.1:8700`; the encoder publishes none |
| Encoder reachability | on `encoder_net`, an `internal: true` network: no route out, no route in, only the CLM service shares it |
| Authentication | `CLM_API_KEY` on the CLM service (section 3); the encoder has no key, the network is its barrier |
| Privileges | `cap_drop: ALL`, `no-new-privileges`, read-only root file system, small tmpfs for caches, no `privileged`, no host PID or network namespace, no device mapping |
| Accelerators | none: CPU only; `CUDA_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES` and `ROCR_VISIBLE_DEVICES` are `-1` and the entrypoint refuses any other value, so an AMD GPU cannot be selected |
| Models | bind-mounted read-only; verified against the registry and `config/pins.txt` before serving |
| Restart | `on-failure:3`; a refused start (exit 78) stays down after three tries |
| Memory | `mem_limit` equals `memswap_limit` in both profiles (no swap); every number is labelled emulated |

## 2. Surfaces disabled, and why

| Surface | How it is off | Reason |
| --- | --- | --- |
| CLM playground (web UI at `/`) | `--no-ui` | an interactive page is not part of a headless service |
| Browser CORS | `--cors` is never passed | the service leaves it off unless asked; a key in a header would otherwise be usable from any page |
| Reference head download | `--ckpt` plus `--no-download` | without them the service downloads the head and its helper sends a HEAD request to the Hub's `config.json` to feed the download counter |
| vLLM usage statistics | `VLLM_NO_USAGE_STATS=1`, `VLLM_DO_NOT_TRACK=1`, `DO_NOT_TRACK=1`; the entrypoint refuses to start unless the first is `1` | vLLM reports usage to `stats.vllm.ai` unless told not to |
| Hub telemetry and online lookups | `HF_HUB_DISABLE_TELEMETRY=1`, `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1`; the encoder is served from a local directory | no model id is resolved on the network |
| GPU paths | CPU target (`VLLM_TARGET_DEVICE=cpu`), no devices | the CPU-first resource rule, and the owner rule that no AMD GPU is used |
| `pip`, package indexes | not used: the wheel is hash-checked and unpacked, its dependency list is not installed | an install step could pull unreviewed code at start |
| Other models | the registry check names one head and one encoder; the model directory must hold exactly the pinned files | an extra safetensors file would be loaded by vLLM; an extra head would be served by the service |
| One-shot setup container | none exists | owner rule: checks and the optional fetch run in the two main services |

## 3. Kept, and how it is guarded

- **CLM service on `127.0.0.1:8700`.** `POST /v1/rank` and `POST /v1/systemone` are the scorer endpoints; `GET /v1/models`
  needs the key; `GET /health` needs none (it reports occupancy and whether the encoder answers, no request data).
  The key is compared by the service to the `Authorization: Bearer` header. The entrypoint refuses an empty key
  (unless `ALIXAI_CLM_ALLOW_NO_KEY=true`, section 7), a placeholder, whitespace and fewer than 32 characters.
- **vLLM on `encoder_net:8090`.** vLLM serves the whole OpenAI-compatible surface; the CLM service uses `/v1/embeddings`
  and the health check uses `/health`. The CLM service sends no key to its embedder (service 0.1.0), so a vLLM
  `--api-key` would break the pair. Network isolation is the control instead: a process outside the two containers
  cannot reach the port, and a process inside the encoder cannot reach out. Only the CLM service shares the network,
  so a compromise of the CLM service reaches the encoder; the CLM service holds no secret besides its own key.
- **Request bodies.** The state text reaches the CLM service and the encoder. The service logs warnings only
  (`log_level="warning"`), vLLM runs with `--disable-log-stats` and without request logging; the entrypoint passes no
  logging flag that would record bodies. Container logs are still not under AlixAI's redaction rules: keep
  them local and short-lived.

## 4. Supply chain

- **Image.** `docker.io/vllm/vllm-openai-cpu:v0.31.0@sha256:495c84fe30f3572b83a78100a1e8ba423dbb77fc532ecefebe114e6b77327bd9`,
  the multi-arch index; the digest was read from the registry's `docker-content-digest` header on 2026-10-07 and is the
  only reference compose pulls. `:latest` and the nightly tags are never used. The same image runs both services, so
  the CLM service adds no second image to pull or scan.
- **Weights.** Qwen3-8B at commit `b968826d9c46dd6066d109eabc6255188de91218` (twelve files, sha256 each in `config/pins.txt`),
  CLM-v0.1-8B at commit `e939398d4556fcd9400c76fa8c5a513202f42b0a` (`CLM_v0.1-8B.pt`, sha256 `b2b4a8c9...eda5`). The LFS
  hashes are the Hub's published values; the small text files were hashed over the bytes at the pinned commit.
  Every start re-hashes all of them. A `.pt` checkpoint is a pickle: loading it executes code, which is why the file is
  hash-pinned and read-only, and why a different head is refused instead of served. The registry's licence review
  (D-H01) covers the weights; the Hub metadata and the repositories declare Apache-2.0 for the head, the code and the encoder.
- **Service code.** `contrastive-lm` 0.1.0, wheel sha256 `7f3eed12...055f`, unpacked from a hash-checked file. Its dependencies
  (torch, fastapi, uvicorn, numpy, requests) are the ones in the pinned image, not fetched.
- **Registry as authority.** `entrypoint.sh` compares the registry entry's `artifact.revision` and `artifact.sha256` with
  the pin for the file the entry names, and refuses on a missing, unpinned, rejected, retired or disagreeing entry.
  Until D-11 adds the entries, the sidecar refuses to start.
- **Not done.** No signature check of the image (the digest is a pin, not a signature), no SBOM, no vulnerability scan of
  the 1.5 GB image: record them in the update procedure when the release process exists.

## 5. Residual risks

- **Root inside the container.** The image defines no user. `cap_drop: ALL`, a read-only root, read-only model mounts
  and `no-new-privileges` bound what root can do. A non-root `user:` is the next step and is untested: libraries that
  look up the account of the user id may fail for an unnamed id.
- **Rootful Linux bind mounts.** With every capability dropped, root cannot write to a host directory owned by
  another user. The data directories are read-only for the services, so only `compose.fetch.yaml` is affected; run
  `./fetch.sh` on the host instead.
- **No seccomp relaxation.** vLLM's CPU documentation describes `--cap-add SYS_NICE` and `seccomp=unconfined` only to
  enable NUMA optimisations; neither is set. Expect `get_mempolicy: Operation not permitted` warnings.
- **Pickle and wheel trust.** Covered by the pins; a compromised publisher at the pinned commit would not be detected
  by the hash, only by review.
- **Memory pressure.** The encoder weights are about 15.3 GiB. Under a cap that is too small, the kernel kills the
  process; the CLM service reports the encoder unreachable and AlixAI falls back with `unavailable` or `resource_gate`.

## 6. Outbound network calls

Three, none at request time:

1. **Model and wheel download**, once per file, by `./fetch.sh` on the host or by `compose.fetch.yaml`: HTTPS GET of the
   exact urls in `config/pins.txt` (`huggingface.co`, which redirects to Hugging Face's content hosts, and
   `files.pythonhosted.org`). After the files exist, the services make no download call.
2. **Image pull**, once per digest: `docker.io` (registry-1.docker.io and its blob hosts), by the host's Docker or Podman.
3. **Telemetry: off.** vLLM usage statistics, Hub telemetry and the CLM download counter are disabled; the encoder has no
   route out at all. `./smoke.sh` and D-08 should confirm this with a packet capture or a deny-all egress rule on the
   run; this page does not claim it was observed.

There is no call to an AlixAI-operated service, to a model provider, or to any host at request time.

## 7. Authentication gap

`adapters/decision/clm` (D-05) has no API key setting: its configuration holds an endpoint, TLS, model, revision and
limits. Since D-14 the adapter resolves `api_key_ref` and sends the key as a Bearer header on `/v1/rank`, `/v1/systemone` and `/v1/models` (not `/health`), so a keyed CLM service accepts AlixAI's calls. `ALIXAI_CLM_ALLOW_NO_KEY=true` with an empty key remains possible only for a manual run on a single-user machine, where the loopback-only publish is the only barrier: do not use it on a shared machine.

## 8. Re-audit on every bump

Resolve the new digest and revisions from the registries, update `compose.yaml`, `config/pins.txt` and the registry entry
in one change, check the new vLLM release notes for new default outbound calls, new endpoints and changed pooling flags,
re-read `clm-serve --help` for new options that open a port or a download, and re-run `./smoke.sh` on the emulated profile.
