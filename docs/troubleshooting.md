# Troubleshooting

Start with `docker compose logs -f`. The default `LOG_LEVEL=warn` is quiet by
design; set `LOG_LEVEL=debug` and restart if you need to see per-request lines
and the exact argv handed to each subprocess.

---

## All my jobs vanished after upgrading from mediatool

**Symptom.** You upgraded from an image that was still called `mediatool`, the
container starts fine, and the job list is empty. You are also logged out.

**Cause.** The project was renamed. The default database path moved from
`/data/db/mediatool.db` to `/data/db/aiofiles.db`, so the app finds no file there
and creates an empty one; the old database is still sitting next to it, untouched.
The session cookie was renamed from `mediatool_session` to `aiofiles_session` at
the same time, which is why your login did not survive - log in again and that part
is done.

**Fix.** Stop the container and rename the database, along with its `-wal` and
`-shm` siblings if they are there:

```sh
docker compose stop
cd ./data/db
mv mediatool.db     aiofiles.db
mv mediatool.db-wal aiofiles.db-wal   # only if it exists
mv mediatool.db-shm aiofiles.db-shm   # only if it exists
cd -
docker compose up -d
```

Do not skip the two siblings if they exist: a `-wal` file holds committed
transactions that are not in the main file yet, and leaving it behind loses them.
If you would rather not move anything, add `DB_PATH: /data/db/mediatool.db` to the
`environment:` block in `docker-compose.yml` - the shipped file does not pass
`DB_PATH` through, so putting it in `.env` alone does nothing.

The first boot will already have created an empty `aiofiles.db`. `mv` replaces it,
which is what you want - there is nothing in it. This is a one-time rename; once
the file is moved there is no further migration to do.

---

## Permission denied on /data after changing PUID/PGID

**Symptom.** Jobs fail with `permission denied` on a path under `/data`, or the
container will not start at all with `create dir /data/...: permission denied`.
It worked before you edited `.env`.

**Cause.** The startup chown is not recursive. The init script fixes `/data` and
its four subdirectories and stops there, because walking a downloads volume with
thousands of files on every boot is a slow way to start a container. Files
written under the old uid still belong to it.

**Fix.** One boot with the repair pass, then turn it off:

```sh
echo "FIX_OWNERSHIP=1" >> .env
docker compose up -d          # watch for "recursively chowning /data"
# wait for it to finish, then edit .env back to FIX_OWNERSHIP=0
docker compose up -d
```

Leaving it at `1` permanently is not harmful, just slow - you pay for the walk on
every restart.

---

## Login always fails after setting AUTH_PASSWORD_HASH

**Symptom.** The password is definitely right and login returns 500
`internal_error`, or 401 no matter what you type. Logs show a malformed argon2id
hash.

**Cause.** `.env` quoting. An argon2id hash is
`$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>` and Docker Compose interpolates
`$NAME` inside unquoted `.env` values. `$argon2id` expands to nothing, so the app
receives a truncated string that is not a valid hash. Verification treats a
malformed hash as an error rather than a quiet "wrong password", which is why you
get a 500 instead of a 401.

**Fix.** Single-quote it. Not double quotes - those still interpolate.

```ini
AUTH_PASSWORD_HASH='$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA'
```

Check what actually arrived:

```sh
docker compose exec aiofiles sh -c 'printf %s "$AUTH_PASSWORD_HASH"' | wc -c
```

A hash from `-hash-password` is 97 bytes. If you see far fewer, the quotes are
missing. `AUTH_PASSWORD_HASH` wins over `AUTH_PASSWORD` when both are set, so a
broken hash is not rescued by leaving the plaintext in place - clear one or the
other.

---

## Container starts, then exits, then starts again

**Symptom.** A restart loop. `docker compose ps` shows the container flapping.

**Cause.** Something failed during boot, and the image is configured to stop
rather than run half-up (`S6_BEHAVIOUR_IF_STAGE2_FAILS=2`, plus `finish` scripts
that halt the container when either long-running service dies). The reason is in
the logs, near the top of each attempt.

The usual culprits:

- `FATAL: AUTH_USERNAME is set but neither AUTH_PASSWORD_HASH nor AUTH_PASSWORD is.`
- `AUTH_USERNAME and AUTH_PASSWORD_HASH must both be set, or both empty` - you
  cleared the username but left the hash, or vice versa.
- `DEFAULT_RETENTION_DAYS must be one of 0, 1, 7, 30 (got 14)` - the retention
  menu is a closed set. See [configuration.md](configuration.md).
- `unknown LOG_LEVEL "verbose"` - it is `debug`, `info`, `warn` or `error`.
- `MAX_CONCURRENT_JOBS must be >= 1`.
- `MAX_UPLOAD_MIB must be between 0 (unlimited) and …`, or `FATAL:
  MAX_UPLOAD_MIB must be a whole number of MiB` - see below.
- `XACCEL_PREFIX must start with / or be empty`, `FATAL: DOWNLOAD_DIR must be an
  absolute path` - both are pasted into the nginx config, so they are checked
  before it is rendered.
- `FATAL: generated /etc/nginx/nginx.conf does not parse` - only reachable if you
  have modified the config template.

**Fix.** Read the message; they all name the variable. `docker compose logs
--tail=50` right after a failed `up -d` is usually enough.

---

## yt-dlp fails on a site that used to work

**Symptom.** A download job fails with `yt-dlp failed (exit 1): ERROR:
[youtube] …: Unable to extract …`, or a probe returns 502 `probe_failed` with
similar text. Other sites still work.

**Cause.** Extractors break when sites change. yt-dlp ships fixes within days;
this image pins a version at build time and never self-updates, because a tool
that rewrites its own binary at runtime is not something you want inside a
container you thought was immutable.

**Fix.** Rebuild with a newer yt-dlp. Pick a release from
<https://github.com/yt-dlp/yt-dlp/releases>, take the checksums for the
`yt-dlp_musllinux` and `yt-dlp_musllinux_aarch64` assets from that release's
`SHA2-256SUMS`, and update the `YTDLP_VERSION` / `YTDLP_SHA256_*` ARGs at the top
of `Dockerfile`. Then:

```sh
docker compose build --no-cache aiofiles && docker compose up -d
```

If you skip the checksums the build fails rather than shipping something
unverified. That is deliberate; do not work around it.

Before you rebuild, rule out the boring causes - the site may want cookies or a
login, may be geo-blocked from your host, or may be rate-limiting you. Reproduce
by hand to find out:

```sh
docker compose exec aiofiles yt-dlp -J --no-playlist -- 'https://…' | head -c 400
```

Note that there is no way to pass cookies, proxies or arbitrary yt-dlp flags
through the API. That is the point of the allow-list, and it means some sites are
simply out of reach for this tool.

---

## Jobs are "failed: interrupted by restart"

**Symptom.** After a restart, every job that was running or queued shows as
failed with exactly that message.

**Cause.** Intended behaviour. Their child processes died with the old container,
and there is nothing to resume - a half-downloaded yt-dlp temp directory and a
half-finished two-pass encode are not restartable. On boot, every row still in
`running` or `queued` is flipped to `failed`.

**Fix.** Resubmit them. There is no retry button and no automatic requeue.

If this keeps happening without you restarting anything, the container is dying
on its own. Check `docker compose logs` for an OOM kill (add a `mem_limit` if a
pathological input is exhausting the host) and check whether one of the two
services is crashing - either one halts the whole container by design.

**Related.** A `docker compose stop` with a browser tab holding the SSE stream
open can end as a hard kill rather than a clean shutdown, because the graceful
HTTP shutdown waits on that long-lived connection while Docker's stop grace
period runs out. If you care about in-flight jobs ending as `canceled` rather
than `failed`, close the tab first or raise `stop_grace_period` in the compose
file.

---

## Uploads are rejected

**Symptom A.** JSON `{"error":{"code":"file_too_large","message":"file exceeds
the N byte upload limit"}}`.

That is the app's own limit. Raise `MAX_UPLOAD_MIB` and restart; nginx's
`client_max_body_size` is derived from it automatically (value + 64 MiB), so
there is no second place to change.

**Symptom B.** An nginx HTML error page - "413 Request Entity Too Large" with no
JSON - or the upload dies partway with no useful message.

That is not this container's nginx: its limit is deliberately 64 MiB *above* the
app's so the app answers first. It is a reverse proxy in front of you with its own
default. Caddy defaults to unlimited but Traefik, another nginx, or a cloud load
balancer will have opinions. Raise the limit there too.

**Symptom C.** The container will not start and the log says `MAX_UPLOAD_MIB must
be between 0 (unlimited) and …`, or `FATAL: MAX_UPLOAD_MIB must be a whole number
of MiB (got '4G')`.

Only whole numbers of MiB are accepted, and only 0 or above. `0` is the way to ask
for unlimited: the app skips its size check and nginx gets `client_max_body_size
0`, which disables its own. Both ends move together, so there is no setting where
one of them quietly rejects an upload the other allowed.

Uploads stream straight to disk, so a large limit costs disk, not memory. Note
that an uploaded file is not deleted when its job finishes - only when the job is
deleted or swept by retention. If `./data/uploads` is growing, that is why.

---

## Running behind a reverse proxy

The app speaks plain HTTP, has no TLS, and has no brute-force protection beyond a
per-IP login limiter. Fronting it with Caddy, Traefik or another nginx is
reasonable. Start by telling the app your proxy exists:

```
TRUSTED_PROXIES=172.18.0.0/16
```

Nothing outside that list (plus loopback, where the bundled nginx lives) has its
`X-Forwarded-*` headers believed, which is what the next two problems are about.
Four things bite.

**Progress bars never move, and the header never leaves "connecting".** Your
proxy is buffering the SSE stream. The bundled nginx turns off `proxy_buffering`
and `proxy_cache` for `/api/events`, keeps the response chunked so an outer hop
can forward it a piece at a time, and re-emits the app's `X-Accel-Buffering: no`
(nginx consumes that header rather than passing it on, so it has to be set again
on the way out). An outer nginx honours it; Caddy and Traefik do not, but they do
not buffer a chunked `text/event-stream` either. If yours does, that is
`proxy_buffering off` plus a `proxy_read_timeout` longer than your longest
transcode. A buffered stream produces no error anywhere - `EventSource` fires
neither `onopen` nor `onerror` - so a UI that loads fine but never updates is the
symptom to look for.

**Login succeeds and immediately bounces back to the login page.** The session
cookie is issued with `Secure` only when the app can tell the original request was
HTTPS - direct TLS, or `X-Forwarded-Proto: https` *from a trusted proxy*. The
bundled nginx passes your proxy's value through when the peer is in
`TRUSTED_PROXIES` and overwrites it with `http` when it is not, so a proxy that
terminates TLS but is not listed leaves the app believing the browser is on plain
HTTP: no `Secure` on the cookie, and a `POST` from an `https://` page counted as
cross-origin (403 `cross_origin`) on any browser that does not send
`Sec-Fetch-Site`. The broken direction is the reverse - a proxy claiming `https`
while you browse over plain HTTP, in which case the browser silently drops the
cookie and every request looks unauthenticated. Make the header match reality,
and list the proxy.

**Login rate limiting counts the wrong address.** The limiter walks
`X-Forwarded-For` from the right and takes the first hop that is not a trusted
proxy, then falls back to `X-Real-IP` and the socket. A proxy that is not in
`TRUSTED_PROXIES` is the first untrusted hop itself, so every attempt looks like
it came from one address and five failures lock out everyone. Add it to
`TRUSTED_PROXIES` and make sure it sets the header from the real connection. A
client-supplied `X-Forwarded-For` passed through unmodified is harmless: forged
entries land to the left of what your proxies append, and the walk stops before
reaching them.

**Large uploads.** See the previous section - the outer proxy's body limit and
read timeouts apply too.

Finally: the shipped port mapping is `127.0.0.1:1144:8000`. If you put a proxy on
the same host, keep it. Only drop the `127.0.0.1:` prefix if something other than
a local proxy genuinely needs to reach the port, and never without
`AUTH_USERNAME` set.

---

## The proxy works but the IP does not (or vice versa)

**Symptom.** `https://aio.example.com` works, `http://192.168.16.35:19882`
returns a bare 403, or an unexpected name does.

That is `PROXY_ONLY=1` and/or `ALLOWED_HOSTS` doing their job - see
[configuration.md](configuration.md#reverse-proxy-trusted_proxies-allowed_hosts-proxy_only).
The API answers with JSON:

```json
{"error": {"code": "host_not_allowed", "message": "this instance does not answer to that address"}}
```

and the frontend gets nginx's plain 403, because the init script renders the same
rules into an nginx `map` - otherwise the UI would load and then fail on every
call.

Things worth checking when it refuses a name it should accept:

- `ALLOWED_HOSTS` entries carry no port and no scheme. `aio.example.com:8443`
  fails startup; the port is stripped before matching.
- `*.example.com` does not match the bare `example.com`. List both if you need
  both.
- The name the app sees is `X-Forwarded-Host` if your proxy sets it, otherwise
  `Host`. A proxy that rewrites `Host` to its upstream (`127.0.0.1:1144`) and
  sets neither is invisible from here - check with `LOG_LEVEL=debug`, the
  rejection is logged with the host it decided on.
- With `PROXY_ONLY=1` and no bundled nginx, the peer has to be in
  `TRUSTED_PROXIES` or the request is refused whatever the name is.

The container healthcheck is not affected: it goes through a loopback-only
listener on port 8001 that skips the guard. If the healthcheck fails after
setting these, it is not the guard.

Neither setting replaces `AUTH_USERNAME`. `Host` comes from the client, so this
stops browsers and casual scans, not someone who can reach the port and craft
headers.

---

## Downloads return 404 or an empty file

**Symptom A.** 404 `{"error":{"code":"file_missing","message":"the output file is
no longer on disk"}}` on a job that says `done`.

The retention sweeper got it, or something removed it from the host side. Check
`expires_at` on the job. Jobs created with a 1-day retention are gone a day after
they were *created*, not a day after you last looked at them.

**Symptom B.** 200 with an empty body, or an nginx 404, on a job whose file you
can see on disk.

`X-Accel-Redirect` is pointing somewhere nginx cannot serve. Inside this container
that should not happen any more - the init script renders the `location` and its
`alias` from `XACCEL_PREFIX` and `DOWNLOAD_DIR`, the same variables the app reads,
and logs the pair at boot:

```sh
docker compose logs aiofiles | grep "X-Accel location"
# init-aiofiles: nginx X-Accel location = /_protected/ -> /data/downloads/
```

If that line does not match what the app is using, one of the two variables
reached only one process - most often because you set it on the app but not in the
container's environment. If you are running your own nginx in front of the binary,
its `location` and `alias` are yours to keep in step.

Either way, `XACCEL_PREFIX=` (an empty value, not a missing one) makes Go serve
the bytes itself. Slower, but correct anywhere.

---

## "Cancel" returns 409 not_cancelable

Only a *running* job can be cancelled; cancellation works by killing a live
process group, and a queued job has no process yet. There is no way to pull a job
back out of the queue.

Delete it instead. The worker will eventually pick up an id whose row is gone,
log it, and move on.

---

## A compress job fails instead of encoding

**"cannot aim at a file size: the length of this file is unknown"** - ffprobe
could not read a duration from the input, and there is no way to divide a byte
budget over an unknown running time. Quality-target compression (`light`,
`balanced`, `aggressive`) does not need a duration and will still work.

**"a 5 MB target cannot hold 2 hours 14 minutes of video at 128 kbps audio"** -
the arithmetic leaves under 100 kbps for video, which is not worth encoding.
Raise the target, drop the audio bitrate, or accept that the file is too long.

Size targets also refuse `video_codec: "copy"`, for the obvious reason: a copied
stream keeps whatever bitrate it already had.

---

## An AVIF or HEIC output is secretly a PNG

**Symptom.** An image job "succeeds", the file has the right extension, and
nothing will open it as AVIF.

**Cause.** On Alpine 3.23 and later libheif is plugin-based and the base package
pulls in decoders only. Without an encoder plugin, `magick in.png out.avif` exits
0 and writes a PNG with an `.avif` extension. No error, no warning.

**Fix.** The image installs `libheif-aom` and `libheif-x265` for exactly this
reason. If you have been editing the Dockerfile's package list, put them back -
and verify with `magick identify -format %m`, never with the exit code, because
the exit code lies here.

---

## "not authorized by the security policy"

**Symptom.** An image job fails with `magick: attempt to perform an operation
not authorized by the security policy` naming a coder such as `MVG` or `PDF`.

**Cause.** Working as intended. `magick` picks its decoder from the uploaded
bytes rather than from the file name, so the image ships
`/etc/ImageMagick-7/policy.xml` (source: `docker/policy.xml`) which denies every
coder and re-allows only JPEG, PNG, WebP, AVIF, HEIC, GIF and TIFF - the formats
the app actually offers. The denied ones are the ones that shell out to
delegates, fetch URLs or read arbitrary paths. SVG is denied there too: uploaded
SVGs are rendered by `rsvg-convert` before ImageMagick sees them, and an SVG
output is written by the app itself (`svg_trace` through potrace).

Note for anyone editing that file: an apostrophe or a backtick in a comment
makes ImageMagick's XML reader stop, silently dropping every policy below it.
`magick -list policy` shows what actually loaded.

**Fix.** Convert the file to a supported format before uploading it. Adding a
coder back means editing `docker/policy.xml` and rebuilding, and is only safe for
a format with no delegate and no external references. `magick -list policy`
inside the container prints what is in force; if it prints nothing, the config
path moved and the policy is not being read at all.

The matching `-limit` flags in `internal/runner/image.go` exist because
`policy.xml` only ships in the image - a local `go run` gets its limits from
argv. A large-but-legitimate image that trips `area`, `width`/`height` or `time`
needs both raised, not one.
