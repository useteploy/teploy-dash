# Getting your data back

**This page has not been rehearsed yet.** The file names and commands are checked
against the code, but nobody has walked these steps end to end on a scratch machine.
Do that once before you rely on it.

## What you need

- **Your recovery key file**, the one you saved during setup. It holds a line starting
  `AGE-SECRET-KEY-`, and comment lines naming your bucket, endpoint and base folder. It
  is the only thing Dash never keeps a copy of. Nobody can open your backups without it.
- **A way to read the bucket**: a read-only key, below.
- **The teploy CLI** and the `aws` CLI, on any machine.

### Making a read-only key

Setup never shows you one, because servers each have their own limited key and your
temporary key is gone. Make one when you need it:

1. Sign in at backblaze.com and open **Application Keys**.
2. Choose **Add a New Application Key**.
3. Under **Allow access to Bucket(s)**, choose your bucket. Under **Type of Access**,
   choose **Read Only**. Leave the file name prefix empty unless you want one server.
4. Copy the key ID and the application key. The second is shown once.
5. Your endpoint is on the bucket's page and in your recovery key file. It looks like
   `https://s3.us-west-004.backblazeb2.com`.

Delete the key in the same place when you are done.

Below, `B` is your bucket, `E` your endpoint, and `recovery.txt` your key file.

## Everything is lost

No servers and no Dash, only the bucket and your key file. Each server's backups are
under `teploy/<server>/`.

1. See what is there:

   ```sh
   export AWS_ACCESS_KEY_ID=<key id> AWS_SECRET_ACCESS_KEY=<application key>
   aws s3 ls --recursive s3://B/teploy/ --endpoint-url E
   ```

2. Fetch and decrypt any object. This needs only the bucket key and your key file:

   ```sh
   TEPLOY_S3_ACCESS_KEY=<key id> TEPLOY_S3_SECRET_KEY=<application key> \
     teploy backup fetch s3://B/teploy/prod/blog/accessories/db/<id>.pgdump.age \
     --identity recovery.txt --endpoint E --output blog-db.pgdump
   ```

   A database dump comes out as a normal dump file, and volumes as a `.tar.gz`.

3. To put it back on a new server, set the server up with `teploy setup`, deploy the
   app, then restore directly from the bucket with your key (next section).

## Moving server A's data onto server B

The server you restore on does not need to be the one that made the backup. Restore
runs on it and places your key there for that one command:

```sh
teploy backup restore <id> --app blog --accessory db --identity recovery.txt \
  --bucket B --endpoint E --prefix teploy/A --host B-server
```

Volumes are the same without `--accessory`. Adopted containers take `--adopted
--accessory <container>`, and `--database <name>` picks one database. Use
`teploy backup list` the same way to find the `<id>`.

Server A's own key is limited to `teploy/A/`, so server B's key cannot read those
objects. Give this command the read-only key in `TEPLOY_S3_ACCESS_KEY` and
`TEPLOY_S3_SECRET_KEY`.

## Dash itself

Dash's own state is in a backup when Dash runs from the deployment template and a
recovery key is set. It appears on the Backups page as `dash/volumes`. Your recovery key
opens that backup on its own, with no need for the lost machine's identity. Dash's
volumes hold its SSH key, so with no recovery key Dash leaves them out on the server it
can prove hosts it, and warns on any server where it cannot tell.

| Path | What it is | Template volume |
|---|---|---|
| `<data dir>/auth.json` | accounts and password hashes | `dash-state` |
| `<data dir>/sessions.json` | sign-ins | `dash-state` |
| `<data dir>/homepage.json` | the homepage layout | `dash-state` |
| `<data dir>/backups.json` | this page's state, never a credential | `dash-state` |
| `<data dir>/incidents.json` | open alert incidents | `dash-state` |
| `<data dir>/silences.json` | alert silences | `dash-state` |
| `<data dir>/audit.jsonl` | the audit trail | `dash-state` |
| `<data dir>/domain-health.json` | the last domain sweep | `dash-state` |
| `<data dir>/sources/` | git sources | `dash-state` |
| `<data dir>/operations/` | the operation journal | `dash-state` |
| `~/.teploy/notifications.json` | alert channels and their secrets | `teploy-config` |
| `~/.teploy/servers.yml` | your server list | `teploy-config` |
| `~/.ssh/id_ed25519`, or `TEPLOY_SSH_KEY` | how Dash reaches your servers | `ssh-key` |

The template's `<data dir>` is `/var/teploy-dash`.

To rebuild:

1. Fetch the newest backup. `dash` runs on one of your servers, called `S` here:

   ```sh
   aws s3 ls s3://B/teploy/S/dash/volumes/ --endpoint-url E
   TEPLOY_S3_ACCESS_KEY=<key id> TEPLOY_S3_SECRET_KEY=<application key> \
     teploy backup fetch s3://B/teploy/S/dash/volumes/<newest>.tar.gz.age \
     --identity recovery.txt --endpoint E --output dash-volumes.tar.gz
   ```

2. On the new machine, deploy Dash from its template once, then stop it and unpack over
   its volumes:

   ```sh
   teploy stop --app dash
   tar -C /deployments/dash/volumes -xzf dash-volumes.tar.gz
   chmod 600 /deployments/dash/volumes/ssh-key/id_ed25519
   teploy start --app dash
   ```

3. Sign in as before, and check the Backups page reads every server.
4. In Settings, use test-send on one alert channel. A Dash that lost its channels is a
   fleet nobody is watching.

### When Dash is not from the template

The Backups page lists any path no backup covers. Copy those yourself, encrypted to your
recovery key alone. Fill in `RECIPIENT` with your `age1...` public key, and run this from
cron on the Dash machine:

```sh
#!/bin/sh
set -eu
RECIPIENT=age1...
STAMP=$(date -u +%Y%m%d-%H%M%S)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
tar -C / -czf "$WORK/dash-state.tgz" var/teploy-dash root/.teploy/notifications.json \
  root/.teploy/servers.yml root/.ssh/id_ed25519
age -r "$RECIPIENT" "$WORK/dash-state.tgz" > "$WORK/dash-state-$STAMP.tgz.age"
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... aws s3 cp \
  "$WORK/dash-state-$STAMP.tgz.age" "s3://B/dash-state/dash-state-$STAMP.tgz.age" --endpoint-url E
```

Adjust the paths to your install. `teploy backup fetch` opens these too.

## Hidden copies

When retention removes a backup, or a compromised server tries to destroy one, Backblaze
only hides it: server keys cannot truly delete. Hidden copies stay for 14 days unless you
changed that under Advanced, then the bucket's lifecycle rule removes them.

If backups disappeared within that window:

1. **Act first, then investigate.** The clock is running on each copy.
2. Sign in at backblaze.com, open your bucket with **Browse Files**, and turn on
   **Show hidden files**.
3. Unhide what you want back. A read-only key cannot do this, so use the console.
4. If you need longer, raise the days on the `teploy/` rule under **Lifecycle Settings**.
5. Then replace the key of any server you suspect, from the Backups page.

## Other things to know

- Backups made before a recovery key changed still need the older key.
- The binaries are not in any backup. Reinstall them from their releases.
- Your servers keep running and backing up without Dash.
