# Rebuilding Dash from its backup

This is the runbook for losing the machine teploy-dash runs on: with the
recovery key, the bucket credentials and this page, a clean machine runs the
fleet's dashboard again. It is linked from **Backups → Dash itself**.

Live rehearsal is deliberately out of scope for this document's first
revision: the paths below are unit-checked against the code
(`internal/server/backuprunbook_test.go` proves every file this page names is
a file Dash actually writes), but the steps have not been walked end-to-end on
a scratch machine yet. Do that rehearsal before relying on it (see
AUDIT_OPEN).

## What a rebuild needs

- The **owner recovery key** (`teploy backup keygen` output, kept offline) —
  the private key `AGE-SECRET-KEY-...`.
- The **bucket credentials** (access key + secret) — the same pair the fleet's
  servers back up to, or a read-capable key for the same bucket.
- The **Dash backup object** from the bucket: the `dash-state` archive made by
  the job below (or your interim equivalent).

## What the backup must contain

Everything below is dash-side state; none of it exists anywhere else.

| Path | What it is |
|---|---|
| `<data dir>/auth.json` | accounts, bcrypt hashes, session token hashes (X03 principals) |
| `<data dir>/sessions.json` | persisted sessions (sign-ins survive a rebuild) |
| `<data dir>/homepage.json` | the homepage layout |
| `<data dir>/backups.json` | Dash Backups state: storage record, recovery public key, drill, auto restore tests, migration evidence |
| `<data dir>/incidents.json` | open alert incidents (so recoveries can close them) |
| `<data dir>/silences.json` | active alert silences |
| `<data dir>/audit.jsonl` | the audit trail |
| `<data dir>/domain-health.json` | the last domain-health sweep |
| `<data dir>/sources/` | registered git sources and their delivery ledgers |
| `<data dir>/operations/` | the durable operation journal |
| `~/.teploy/notifications.json` | alert channels, SMTP/webhook secrets, public URL, heartbeat receivers — **the Dash secrets file** |
| `~/.teploy/servers.yml` | the teploy CLI's server list (teploy-config) |
| the SSH key Dash reaches the fleet with (`~/.ssh/id_ed25519`, `id_rsa`, or `TEPLOY_SSH_KEY`) | fleet access |

The default `<data dir>` is `/var/teploy-dash` (or the directory your service
unit / `--data` flag names — check the unit's ExecStart).

## Taking the Dash backup (interim, until the CLI can do it)

The teploy CLI's policy engine (v0.1.52) cannot yet back up arbitrary
directories, and it always encrypts to the owner key **and** the running
server's own identity — Dash's state must be readable with the owner key
ALONE (a rebuild means the old machine, and its identity, is gone). That is a
CLI gap (see AUDIT_OPEN); until the engine grows those two abilities, the
dash-state backup is a manual job on the Dash host, encrypted with `age` to
the recovery key only:

```sh
#!/bin/sh
# dash-state-backup: run from cron on the Dash host (root or the dash user).
# Keeps 30 daily copies. Fill in RECIPIENT (the age1... public key from
# `teploy backup keygen`) and the bucket settings.
set -eu
RECIPIENT=age1...
STAMP=$(date -u +%Y%m%d-%H%M%S)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
tar -C / -czf "$WORK/dash-state.tgz" \
  var/teploy-dash \               # <- your <data dir>
  root/.teploy/notifications.json \
  root/.teploy/servers.yml \
  root/.ssh/id_ed25519             # <- or your TEPLOY_SSH_KEY path
age -r "$RECIPIENT" "$WORK/dash-state.tgz" > "dash-state-$STAMP.tgz.age"
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... aws s3 cp \
  "dash-state-$STAMP.tgz.age" "s3://BUCKET/dash-state/dash-state-$STAMP.tgz.age" \
  --endpoint-url https://... 
find . -name 'dash-state-*.tgz.age' -mtime +30 -delete
```

The object lands beside the fleet's backups in the same bucket, under its own
`dash-state/` prefix, encrypted to the owner key only. It does not go through
the policy engine yet — when the CLI gains path targets and owner-only
encryption, this job is replaced by a policy entry and this section deletes
itself.

## Rebuilding

1. **Prepare the machine**: install the teploy CLI (same version or newer) and
   teploy-dash (same release or newer). Create the dash user if you run one.
2. **Fetch and decrypt the newest object** (the recovery drill, for real):

   ```sh
   TEPLOY_S3_ACCESS_KEY=... TEPLOY_S3_SECRET_KEY=... teploy backup fetch \
     s3://BUCKET/dash-state/dash-state-LATEST.tgz.age \
     --identity recovery.txt --endpoint https://... --output dash-state.tgz
   ```

   `recovery.txt` is the `teploy backup keygen` output you kept. If this step
   fails, nothing else matters — that is why the Backups page keeps the
   decrypt drill banner up until it has passed once.

3. **Restore the files** (adjust the data dir to your install):

   ```sh
   tar -C / -xzvf dash-state.tgz          # puts files back where the job found them
   chmod 600 ~/.teploy/notifications.json ~/.teploy/servers.yml ~/.ssh/id_ed25519
   chmod 700 ~/.ssh
   ```

4. **Start teploy-dash**. Sign-in works immediately (accounts and sessions
   came back with `auth.json`/`sessions.json`); monitors, restore tests, the
   backups page state, open incidents and silences are all as they were.
   Verify on the Backups page that every server still reads fresh — the
   restored SSH key is what reaches them.

5. **Check the alert path end to end**: Settings → test-send on one channel.
   A Dash that lost its channel config silently is a fleet nobody is watching.

## What is explicitly not covered

- **The teploy CLI and teploy-dash binaries**: reinstall from their releases;
  pin the versions you ran if you care about byte-identical behaviour.
- **The fleet's servers**: they keep running without Dash; their backups run
  from their own cron. Rebuild the machine, not the fleet.
- **Anything outside the table above** (for example a TLS certificate on a
  reverse proxy in front of Dash): reissue it.
