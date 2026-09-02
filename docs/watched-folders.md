# Watched-folder backup

Keep a folder on this machine arriving in a MemQL Library folder, continuously.

The arrangement is made in MemQL OS — **Files → Backups → Back up a folder** —
and this machine does the watching. Engine half: epic memql#4783. This half:
memql#4841.

---

## The rules, before the setup

**One-way, forever.** Files go from this machine to MemQL and never the other
way. Nothing MemQL does can change, move or delete a file here.

**A deletion here flags the copy; it never removes it.** Delete a file from a
watched folder and the copy in the Library is labelled *gone from the machine*
and stays whole and downloadable. That is the point of a backup, and it is the
one rule the whole design is arranged around.

**This machine decides which folders it will honour.** A backup is arranged in
the cluster — from a browser, possibly on a different computer — so the path in
it is one somebody else's screen named for this machine. Nothing is read until
this machine's own `policy.yaml` allows it.

---

## Setting one up

**1. Allow the folder here.** In `~/.memql/policy.yaml`:

```yaml
backup:
  roots:
    - ~/Clients
    - /Volumes/Work
```

Anything at or beneath a listed root may be backed up. The list is empty by
default and an empty list means *nothing*, not *everything*. `SIGHUP` reloads
it without restarting the worker (`kill -HUP $(pgrep -f 'memql worker run')`),
and roots MERGE on reload, so adding one does not un-authorise another.

`fs.deny` still applies: a folder you marked never-touch is not one to upload.

**2. Sign in on this machine.**

```bash
memql login
```

This is separate from pairing, and both are needed. The worker token this
machine runs on authenticates its *stream*; the Library needs a **user**
sign-in, and a PAT will not do (PATs verify only on the identity node). Once
signed in, the session refreshes silently — the worker never opens a browser.

**3. Arrange the backup in MemQL OS.** Files → Backups → *Back up a folder*:
pick this machine, type the folder's full path, choose where it lands.

**4. Check what this machine will do:**

```bash
memql worker backup            # what is arranged, and whether this machine agrees
memql worker backup --once     # run one sweep now
```

`--once` runs exactly the pass the background worker runs. It needs this
machine to have registered at least once — the worker records its registration
id when it connects, and a push cannot name its machine without one. If it has
not, the command says so and does nothing rather than sweeping as nobody.

The listing shows **every** backup you have set up, on any machine, and marks
the ones this machine sweeps. A folder listed for another of your computers is
that computer's to honour.

---

## What it does, and how often

Every five minutes, per watched folder:

1. Check the path against `backup.roots`. Refused → report *this machine said
   no*, with the reason, and stop.
2. Walk the folder. Skip hidden entries (unless the backup says otherwise),
   the usual derived directories (`.git`, `node_modules`, `vendor`, `dist`,
   `__pycache__`, …), and anything the backup's own *Also skip* patterns match.
3. For each file: unchanged size and timestamp → skip. Changed → hash it;
   same hash → record the new timestamp and send nothing; new hash → upload.
4. Report what was found: the file count and total size **at the origin**,
   which is a thing only this machine can answer.
5. Report any file that is no longer where it was, as *gone from the machine*.

Files above 32 MiB upload in resumable 16 MiB chunks, so an interrupted push of
a large file resumes rather than restarting.

**It is a scheduled walk, not a file watcher.** A backup has to reconcile —
everything that changed while this process was down produced no event — and the
liveness check has to look on a schedule anyway. One mechanism, and no watch
limits to exhaust on a deep tree.

---

## When it is not working

Run `memql worker backup`. It answers the three questions in order.

| What you see | What it means |
|---|---|
| `Sign-in: NONE` | Run `memql login` here. |
| `Allowed folders: NONE` | Add the folder to `backup.roots` above. |
| `REFUSED: ...` beside a folder | That path is not under any allowed root, or `fs.deny` covers it. |
| `No watched folders are set up for you` | Nothing is arranged yet — set one up in MemQL OS. |
| `another machine's` beside a folder | That backup names a different computer; only that one sweeps it. |
| `no registered cluster matches this worker's cluster_url` in the log | The worker's `cluster_url` and the entry in `memql cluster list` name different hosts (a port on one and not the other is the usual cause). |
| Nothing arrives, no error | Check the Files app: a backup shows *waiting for this machine* until the first sweep reports. |

A folder listed for **another** of your machines appears here too. Only the
machine a backup names sweeps it.

### In the Files app

Each backup draws as a link between the machine and the Library folder:

- **Backed up** — everything at that path has arrived, as of the last check.
- **Catching up** — this machine has newer files that have not arrived yet.
- **Needs a look** — the folder is missing or unreadable, or files that were
  backed up are no longer at the origin.
- **This machine said no** — `backup.roots` does not list the path.
- **Waiting for this machine** — no sweep has reported yet.
- **Paused** — nothing is being copied; everything already there stays.

The *checked N ago* beside each one is what makes *Backed up* honest: "backed
up, last checked three weeks ago" is a different claim from "backed up".

---

## Not doing this

- **Two-way sync and conflict resolution.** Refused deliberately; that is the
  complexity cliff this stays on the safe side of.
- **Restoring from the Library onto this machine.** Download a file from the
  Files app like any other.
- **Following symlinks out of a watched folder.** A link would let anything on
  the machine be pulled in, which is the policy veto defeated from the inside.
