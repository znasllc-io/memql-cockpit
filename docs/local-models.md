# Local models on the fleet — the machine side

The cockpit can serve MemQL's own operations — planning, conductor/routing,
suggestions, embeddings — from a model running on **this** machine, over the
worker stream it already holds open. Nothing is billed per token and no prompt
leaves the hardware.

The canonical record is the engine's
[`docs/public/operate/local-models.md`](https://github.com/znasllc-io/memql/blob/main/docs/public/operate/local-models.md)
(epic [memql#4676](https://github.com/znasllc-io/memql/issues/4676)). This page
is the half that happens on the machine: what has to be true before it offers
anything, what it tells the cluster, and what to check when it offers nothing.

The one command worth knowing first:

```bash
memql worker models
```

It prints exactly what this machine would advertise, or the reason it would
advertise nothing.

---

## Three things must be true

Nothing is on by default, and each of the three fails in a way that looks
identical from the portal — the machine is simply absent from the model list.
That is what `memql worker models` exists to disambiguate.

### 1. The hardware floor

| Platform | Minimum (supported) | Recommended |
|---|---|---|
| macOS | Apple Silicon (M1+), 16 GB unified memory, macOS 13+ | M2 Pro+ / 32 GB for 8B-class at comfortable latency |
| Linux | x86_64 + discrete GPU with ≥ 8 GB VRAM (CUDA/ROCm) | 12–16 GB VRAM |
| CPU-only / Intel Mac | Not an inference machine | — |

A machine below the floor stays a **full worker for everything else** — shell,
filesystem, HTTP, computer use, local apps — and nothing about it is degraded.
It simply does not appear in the model catalog.

The check runs here rather than in the cluster because only this machine can
see its own GPU; a central check would be guessing from a hostname. On macOS
it reads `hw.optional.arm64`, `hw.memsize` and `kern.osproductversion`; on
Linux it asks `nvidia-smi`, then the amdgpu driver's
`mem_info_vram_total`. **A fact that cannot be established is not a fact in
this machine's favour** — an unreadable probe means "not offered", with the
reason named.

### 2. A runtime with a model in it

**Ollama is discovered natively**, at `http://127.0.0.1:11434` or wherever
`OLLAMA_HOST` points (`host:port`, `http://host:port` and a bare host all
work). `/api/tags` says what is installed; `/api/show` says what each one can
do.

```bash
brew install ollama          # or https://ollama.com/download
ollama serve
ollama pull llama3.1:8b      # the operational class: 7–8B instruct
ollama pull nomic-embed-text # if embeddings should run locally
```

**Any OpenAI-compatible endpoint** works too — LM Studio, vLLM, llamafile, and
Ollama's own `/v1` surface — but it has to be **declared**, because
`/v1/models` returns ids and nothing else. See below.

No runtime is not an error. Most machines in a fleet have none, and that means
"this machine offers no models" — a fact the worker reports, not a condition it
logs every heartbeat.

### 3. `models.allow` in `~/.memql/policy.yaml`

**Default-deny**, the same posture `apps.allow` has and for the same reason:
serving a call spends this machine's own GPU on somebody else's prompt.
An empty allow list is the state of every machine upgrading into this feature,
and it does not mean "all".

```yaml
models:
  allow:
    - llama3.1:8b
    - nomic-embed-text

  # Optional: OpenAI-compatible endpoints, and what they can do.
  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:1234/v1
      api_key_env: LMSTUDIO_KEY      # optional; the VALUE is never logged
      models:
        - id: qwen2.5-7b-instruct
          context_window: 32768
          structured_output: true
          max_concurrent: 2
```

`SIGHUP` reloads it — `kill -HUP $(pgrep -f 'memql worker run')` — so a newly
pulled model becomes offerable without a restart.

A model that is present but **not** listed is still reported, marked blocked.
That is what lets the portal say "present, blocked" instead of rendering it
identically to "not installed": one of those you can fix, the other sends you
looking for the wrong problem.

---

## What this machine tells the cluster

Models ride the **existing** registration mechanism — there is no second
channel — as labels the engine's fleet router selects on:

```
capability   MODEL
label        model:llama3.1:8b   = ctx=131072,structured=1,max=2
label        model:nomic-embed-text = ctx=2048,embeddings=1,max=4
label        runtime:ollama
concurrency  MODEL = 6
```

The value is a flat `k=v` list rather than JSON because labels are
`map[string]string` end to end — the concept, the wire, the Fleet page — and a
JSON blob inside one would be unreadable in every surface that renders labels
as text.

**Every capability defaults to absent, and the direction is deliberate.** The
engine is fail-closed: a model that says nothing about structured output is
never selected for a structured prompt. A probe that cannot establish a
capability therefore claims nothing — because a model that quietly answers
prose to a conductor turn produces a parse failure three layers away, naming
nothing here.

That has one visible consequence worth knowing. Ollama has no "structured
output" capability of its own, so the cockpit claims it only for models that
report **`tools`** — which covers the operational class (`llama3.1:8b`,
`qwen2.5:7b`). If you disagree about a specific model, declare it under an
OpenAI-compatible runtime pointed at Ollama's own `/v1` surface with
`structured_output: true`. Your machine, your claim.

That escape hatch relies on one rule worth knowing: **a model id can be
advertised once**, because the id is the label key. When the native probe and
a declared runtime both offer the same id, the **declared** entry wins and
`memql worker models` says which one was shadowed. If it went the other way,
the file you just edited would change nothing.

`max_concurrent` is the one attribute that is **never** left absent: the engine
reads a missing ceiling as *unlimited*. `OLLAMA_NUM_PARALLEL` sets it; silence
gets 1.

### `sharedInference` is not the cockpit's to send

Offering this machine for the cluster's **system** work — the calls with no
acting user — is an **operator label**, set by the machine's owner in the
portal. It is deliberately not read from what the cockpit reports, because
`labels` is overwritten from `Register` on every reconnect: an opt-in stored
there would be granted by the machine rather than by its owner, and revoked
roughly whenever the lid closed. The cockpit never derives one.

### A changed model set costs a reconnect

Model labels are bound at `Register`, and `Heartbeat` carries apps but no
labels — so a model pulled, removed or newly allowed while the worker is
connected is invisible to the cluster until it registers again, and registering
again means reconnecting.

The worker re-checks its offered set every 60 s and reconnects only when the
**advertised labels actually changed**, only when no tool call, app session or
model call is in flight, and never twice inside two minutes. A model finishing
its pull will not kill an hour-long app session, and a runtime flapping up and
down will not turn this worker into one that reconnects forever.

---

## Serving a call

`ModelCallStart` → deltas → `ModelCallEnd`, correlated by request id, on the
stream that is already open. Both kinds: `chat` and `embedding`.

- **Deltas carry a monotonic `seq`.** The engine drops out-of-order and
  duplicate deltas rather than repairing them.
- **The envelope owns the deadlines.** Timeout, idle ceiling and keepalive
  arrive on the call. A local 8B model on a cold GPU is twenty seconds from
  start to first token — indistinguishable from a wedged machine to anything
  holding only a wall clock — so a call with nothing to say still says it, on
  the keepalive cadence the caller set.
- **Cancel is a real cancel.** The in-flight request to the runtime is
  aborted; a call that merely stopped being read would leave the GPU busy for
  the length of a generation nobody will use. A lost stream and a drain do the
  same thing to every live call.
- **Usage is reported, never inferred.** What the runtime said, including which
  model it actually ran. Silence stays silence, which the cluster records as
  billing "unknown"; a count derived from string length would be stored as
  measured.
- **Both concurrency ceilings are enforced here** — per model and machine-wide
  — from the same numbers the registration advertises. The engine rations too,
  but the advertisement is a claim about this hardware and two replicas
  selecting at the same moment is an ordinary race.

A refusal names its own fix: `model_not_offered`,
`model_concurrency_exceeded`, `schema_unsupported`, `unsupported_kind`,
`duplicate_request`, `runtime_error`, `cancelled`, `timeout`,
`worker_stopped`.

---

## When it does not work

**"My laptop is not in the model list."** Run `memql worker models`. It answers
in the order the causes actually occur:

| What it prints | What to do |
|---|---|
| `Hardware floor: NOT met — …` | Nothing, on this machine. It stays a full worker. |
| `Runtimes: none found.` | Install Ollama and `ollama serve`, or declare an endpoint under `models.runtimes`. |
| `present, BLOCKED` | Add the model to `models.allow`, then `SIGHUP`. |
| `declared runtime … did not answer` | The endpoint is down. Its models are held back deliberately — advertising them would send prompts to a server that is not there. |
| `declared but not offered` | The endpoint is up but is not currently serving that model id. |
| labels printed, still absent in the portal | The machine is registered but offline, or the change has not cost a reconnect yet. Give it a minute; the worker will not interrupt work in flight to re-advertise. |

**A model is offered but never picked.** Read its attribute line. A model with
`structured output: not advertised` is passed over for every planner,
conductor and suggest prompt; one with `context not advertised` meets no
context floor. Both are the fail-closed rule working as intended — the fix is
a runtime that reports the capability, or a declared runtime where you state
it.

---

## Related

- Engine + protocol: [memql#4676](https://github.com/znasllc-io/memql/issues/4676),
  design record `docs/superpowers/specs/2026-08-26-local-models-on-the-fleet-design.md`
- The cockpit half: [memql-cockpit#357](https://github.com/znasllc-io/memql-cockpit/issues/357)
- [`local-apps.md`](local-apps.md) — the same shape, for Claude Code and Codex
