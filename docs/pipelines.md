# Pipeline steps on this machine

Let the cluster run one of its CI pipeline steps here, when the step names a
need the cluster's own runners cannot meet -- a macOS toolchain, a GPU, Docker.

The arrangement is made in two places: the repository's pipeline says a step
may run on the fleet, and this machine's `policy.yaml` says this machine will
take it. Engine half: epic memql#5478 (`workerHost.pipeline_step`, issue
memql#5494). This half: the `pipeline_step` action, the `pipelines` policy and
the `pipelines=allowed` registration label.

---

## The rules, before the setup

**Nothing runs here until this machine's owner says so.** `pipelines.allow` is
off on every machine, including every machine upgrading into this feature. A
laptop is never a default place for somebody's CI.

**Two consents, and this is one of them.** The cluster only routes a step to a
machine that registers the label `pipelines=allowed`, and this worker registers
it exactly when `pipelines.allow` is true -- a `pipelines` label written by hand
into `worker.yaml` is removed, because a machine that would refuse every step
routed on it must not carry it. The other consent is the pipeline's own, on the
cluster.

**It is a standing consent, not a consent window.** A step arrives when a push
lands, not while somebody is at the machine, so `memql worker consent grant`
has nothing to do with it, and `memql worker consent revoke` does not stop it.
Withdraw by removing `allow` (see below).

**A step runs as you.** It is the repository's own command, run as the user
the worker runs as, with that user's environment and files. This is not a
sandbox. `pipelines.allow` says you trust the repositories routed here with
that; `pipelines.repos` says which repositories those are.

---

## Setting it up

In `~/.memql/policy.yaml`:

```yaml
pipelines:
  allow: true
  repos:
    - acme/widgets
    - acme/tools
  workspace_root: ~/ci
  max_timeout_sec: 3600
```

| Key | Default | Meaning |
|---|---|---|
| `allow` | `false` | Whether this machine runs pipeline steps at all. |
| `repos` | empty: any repository | When it lists any, only these repositories' steps run here. `owner/name`, compared without regard to case or a `.git` suffix. |
| `workspace_root` | `fs.workspace_root/pipelines`, else `~/.memql/pipelines` | Where each step's fresh checkout is made, and removed again. |
| `max_timeout_sec` | `3600` | The longest a step may run, whatever it asks for. A step that names no timeout gets the cluster's own default, 1200 seconds, under this cap. |

`SIGHUP` reloads it (`kill -HUP $(pgrep -f 'memql worker run')`). The block
**replaces** on reload rather than merging: removing `allow`, or narrowing
`repos`, takes effect for the next step that arrives. The worker then
re-registers so the cluster's router sees the new answer -- at the first moment
nothing is running, because work in flight is never cut short. A step routed
here on the old answer in the meantime is refused by the new one.

---

## What a step does

1. **Admission.** `allow`, then `repos`. Refused -> `denied_by_policy`, with a
   sentence naming the setting to change. Nothing is created.
2. **A fresh directory**, `step-*` under `workspace_root`, readable only by
   you.
3. **The checkout**, the cluster Job's own clone script: `git init`, a depth-1
   `git fetch` of exactly the commit, `git checkout FETCH_HEAD`.
   - Over `https` only. `ssh` and scp-like URLs would fetch with this machine's
     SSH key, and plain `http` would send the token in the clear.
   - The step's short-lived token travels to git as an `http.extraheader` in
     git's `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_0` / `GIT_CONFIG_VALUE_0`
     environment: never on disk, never on a command line. No token means an
     anonymous fetch of a public repository.
   - Anonymous means anonymous: the fetch reads no global or system gitconfig
     (so no `insteadOf` can turn it into an SSH fetch with your key), resets
     every credential helper, carries none of the worker's `GIT_` variables,
     and never prompts. Proxies set through `HTTPS_PROXY` still apply.
4. **The command**, `/bin/sh -c <command>` in the checkout, in a process group
   of its own. It runs with **this machine's environment** -- its `PATH` and
   toolchains -- plus the step's environment variables and secrets. The one
   thing held back is the worker's own credential (`MEMQL_WORKER_TOKEN`), so a
   test that prints its environment does not print it into a log.
5. **Output**, streamed to the cluster as it is written, stdout and stderr
   apart. Every secret value of four characters or more -- and each line of a
   multi-line one -- is replaced with `***` before it leaves this machine.
6. **The timeout.** Past it, the whole process group is sent `SIGTERM`, and
   `SIGKILL` ten seconds later; the step fails as `timeout`. When the command
   ends, anything it left running in its group is killed, the way a pod's
   teardown ends what its container started.
7. **Artifacts**, the declared paths or globs relative to the checkout,
   directories taken whole, packed as a `tar.gz`. Only regular files travel,
   and never through a link that leads out of the checkout. Up to 64 MiB of
   files and 20 MiB compressed -- what one result can carry on the worker
   stream, whose messages are capped at 32 MiB. Over either, the archive is
   left out and the result says `artifactsTooLarge`; the step keeps its own
   exit status. A declared path nothing matched is listed in
   `artifactsMissing`.
8. **The directory is removed**, whatever happened -- a tree the step made
   read-only included.

The result is the step's exit code (or 128 plus the signal that ended it, as a
shell reports it), its duration, the artifacts and the missing paths:

```json
{"exitCode":0,"durationMs":41250,"artifactsTgzBase64":"H4sI...","artifactsMissing":[]}
```

| Failure | When |
|---|---|
| `denied_by_policy` | `pipelines.allow` is off, or `pipelines.repos` does not list the repository. |
| `bad_request` | The request is malformed: not an https clone URL, not a full 40-character sha, no command, an environment name that is not one, a secret that shadows an environment variable, an artifact path that is absolute or contains `..`. |
| `pipeline_clone_failed` | git is missing (or is the macOS install stub), or the fetch or checkout failed; git's own reason is quoted. |
| `timeout` | The step ran past its timeout and was stopped. |
| `cancelled` | The cluster cancelled the step, the worker's stream to it was lost, or the worker is shutting down. |
| `exec_failed` | The workspace root could not be created, or `/bin/sh` could not start. |

A failing command is **not** a failure here: it is a result with a non-zero
`exitCode`.

---

## The environment a step sees

A step inherits the **worker's** environment, which is not your login shell's.

- **macOS (LaunchAgent).** The worker runs with launchd's `PATH`
  (`/usr/bin:/bin:/usr/sbin:/sbin`). Toolchains under `/opt/homebrew/bin` or
  `/usr/local/go/bin` are not on it. Add their directories to the PATH launchd
  gives your user's services (for example `sudo launchctl config user path
  "/opt/homebrew/bin:/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin"`, then log
  in again), or have the pipeline's command set `PATH` itself. On a Mac without
  the command-line developer tools, `/usr/bin/git` is an install stub that
  would raise a dialog; a step refuses rather than run it.
- **Linux (user systemd unit).** The installer's unit hardens the worker, and a
  step inherits it: `ProtectHome=read-only` leaves only `~/.memql` writable in
  your home, so toolchain caches there (`~/.cache/go-build`, `~/go/pkg/mod`,
  `~/.npm`) are read-only -- point them under `~/.memql` or the checkout from the
  step's command; `MemoryDenyWriteExecute=yes` stops JIT runtimes such as
  Node.js. Relaxing the unit (`systemctl --user edit memql-worker`) is a
  decision about this machine to make deliberately.

`shell.allow`, `shell.deny`, `shell.run_as_user` and the shell's `max_*`
limits do not apply to a step: the command is the repository's own,
`pipelines` is its consent, and `max_timeout_sec` its limit. One caveat: the
worker applies the shell's `max_*` limits to its own process the first time it
runs a `workerHost.exec` call, and every process it starts after that -- a
step included -- inherits them until the worker restarts.

---

## Not doing this

- **A sandbox.** A step runs as you; choose the repositories in
  `pipelines.repos` accordingly.
- **A consent window for steps.** The policy is the consent.
- **Leftover cleanup after a crash.** A worker killed mid-step leaves that
  step's `step-*` directory behind. Anything under `workspace_root` that no
  step is running in can be deleted.
