# Configuration

Everything is configured through environment variables. There is no config file
and no settings screen that writes to disk — if you want to change behaviour, you
change `.env` and restart the container.

Two things to know before the table.

An empty value counts as unset. `config.env()` only accepts a variable if it is
present *and* non-empty, so `LOG_LEVEL=` behaves exactly like deleting the line.
That is why `.env.example` can list every default without changing anything.

Integers that do not parse fall back to the default. `config.envInt()` runs
`strconv.Atoi` and, if it fails, logs a warning naming the variable and uses the
default instead. `MAX_UPLOAD_MIB=4G` gets you 4096 and a line in the log; it is not
a startup failure. A value that parses but is out of range — a negative
`MAX_UPLOAD_MIB`, a `MAX_CONCURRENT_JOBS` of 0 — does stop the container.

## The variables

Read by the Go process (`internal/config/config.go`):

| Variable | Default | What it does |
| --- | --- | --- |
| `AUTH_USERNAME` | *(empty)* | The single login account. Empty disables authentication for the whole API. |
| `AUTH_PASSWORD_HASH` | *(empty)* | argon2id hash in PHC form. Must be set together with `AUTH_USERNAME`. |
| `SESSION_TTL_HOURS` | `720` | Session lifetime. 720 hours is 30 days. Values ≤ 0 are treated as 720. |
| `MAX_CONCURRENT_JOBS` | `2` | Worker goroutines, so also the ceiling on simultaneous yt-dlp/ffmpeg/magick processes. Must be ≥ 1 or startup fails. |
| `QUEUE_DEPTH` | `256` | Buffered channel size. Submissions beyond this are recorded as failed jobs and answered with 503. |
| `DEFAULT_RETENTION_DAYS` | `7` | Retention applied when a job does not specify one. Must be 0, 1, 7 or 30. |
| `MAX_UPLOAD_MIB` | `4096` | Per-file upload cap. Also drives nginx's `client_max_body_size`. `0` means unlimited on both. Negative values fail startup. |
| `TRUSTED_PROXIES` | *(empty)* | Comma-separated IPs or CIDR blocks whose `X-Forwarded-*` headers are believed. Loopback is always trusted on top of this. An unparseable entry fails startup. |
| `ALLOWED_HOSTS` | *(empty)* | Comma-separated names the instance answers to; empty means any. `*.example.com` matches subdomains at any depth, `*` means any. An entry with a scheme, port or path fails startup. |
| `PROXY_ONLY` | `0` | `1` refuses any request whose `Host` is a bare IP address, or that did not arrive through a trusted proxy. |
| `LOG_LEVEL` | `warn` | `debug`, `info`, `warn`/`warning` or `error`. Anything else fails startup. |
| `LISTEN_ADDR` | `127.0.0.1:1144` | Where the Go server binds *inside* the container. nginx's `/api/*` locations proxy to the same address, so changing this moves both ends together — change the port to dodge a clash, but keep the host at `127.0.0.1` unless you know why you're changing it. |
| `DATA_DIR` | `/data` | Root of every other path below. |
| `DB_PATH` | `$DATA_DIR/db/aiofiles.db` | SQLite file. |
| `DOWNLOAD_DIR` | `$DATA_DIR/downloads` | Finished job output. Also becomes the `alias` of the X-Accel location in nginx. |
| `UPLOAD_DIR` | `$DATA_DIR/uploads` | Files uploaded for conversion. |
| `TMP_DIR` | `$DATA_DIR/tmp` | Scratch space for in-progress work. |
| `XACCEL_PREFIX` | `/_protected` | Internal nginx location used to hand downloads off; the same value renders the `location` block. Must start with `/`. Set it to an empty string to stream through Go instead. |
| `YTDLP_BIN` | `yt-dlp` | Resolved through `PATH` unless you give an absolute path. |
| `FFMPEG_BIN` | `ffmpeg` | ditto |
| `FFPROBE_BIN` | `ffprobe` | ditto |
| `MAGICK_BIN` | `magick` | ditto |

`TRUSTED_PROXIES`, `ALLOWED_HOSTS` and `PROXY_ONLY` are read by the init script
as well, which renders the same host rules into nginx so they cover the frontend
and not only the API. `LISTEN_ADDR` is too, so nginx's `proxy_pass` target
always matches where Go actually bound.

Read by the container's init script or the image itself, never by the Go process:

| Variable | Default | What it does |
| --- | --- | --- |
| `PUID` / `PGID` | `1000` / `1000` | UID/GID both long-running services drop to, and the owner of `/data`. |
| `AUTH_PASSWORD` | *(empty)* | Plaintext password. Hashed at boot into `AUTH_PASSWORD_HASH`, then removed from the service environment. |
| `FIX_OWNERSHIP` | `0` | `1` makes the init step recursively chown `$DATA_DIR`. |
| `TZ` | `UTC` | Timezone the container's clock formats log timestamps in. It has no effect on the UI, which renders every time in the browser's own timezone. |

The image also sets `S6_BEHAVIOUR_IF_STAGE2_FAILS=2`, `S6_VERBOSITY=1`,
`S6_KILL_GRACETIME=10000` and `S6_SERVICES_GRACETIME=10000`. You can override
them, but the first one in particular is doing real work — see
[architecture.md](architecture.md).

## The auth pair rule

`config.Load` refuses to start if exactly one of `AUTH_USERNAME` and
`AUTH_PASSWORD_HASH` is set:

```
AUTH_USERNAME and AUTH_PASSWORD_HASH must both be set, or both empty
```

Both empty means authentication is off — every endpoint is open, including job
creation and file download. The init script shouts about this at every boot. Both
set means login is required and there is exactly one account; there is no user
table and no way to add a second.

You have two ways to supply the password. `AUTH_PASSWORD_HASH` is the real input.
`AUTH_PASSWORD` is a convenience: the init script runs `aiofiles -hash-password`
on it, writes the result into the container environment as `AUTH_PASSWORD_HASH`,
and deletes the plaintext from the service environment. The plaintext still sits
in `.env` and in `docker inspect` output, so on a shared host, generate the hash
yourself:

```sh
docker compose run --rm --entrypoint aiofiles aiofiles -hash-password 'your password'
```

Then single-quote it in `.env`. This is not optional. An argon2id hash looks like
`$argon2id$v=19$m=65536,t=3,p=2$…` and Compose interpolates `$NAME` inside
unquoted `.env` values, so an unquoted hash arrives truncated and every login
fails with a hash-decode error. Single quotes turn interpolation off.

If `AUTH_USERNAME` is set and neither password variable is, the init script exits
non-zero and the container stops. That is intentional — it is better than booting
with auth silently disabled.

The hashing parameters are fixed in the binary (64 MiB, t=3, p=2, 16-byte salt,
32-byte key). Verification reads the parameters back out of the stored hash, so an
older hash keeps working if those constants ever change.

## The retention day menu

`DEFAULT_RETENTION_DAYS` accepts only 0, 1, 7 and 30. Anything else fails startup
with a clear message. This looks arbitrary until you see why: the same set is
enforced by `presets.checkRetention` on every incoming job, and the config default
is what fills in `retention_days` when a client omits it. A default of 14 would
mean every request that did not set retention explicitly failed validation with
"must be one of 0, 1, 7, 30" — a confusing failure a long way from its cause. So
the check happens at boot instead.

`0` means keep forever: no `expires_at` is written and the sweeper never looks at
the job. The other values become `now + N days` at submission time, measured from
when the job is *created*, not when it finishes.

## The upload cap

`MAX_UPLOAD_MIB` is enforced in two places and they are deliberately not the same
number. The Go handler wraps the request body in a `MaxBytesReader` at exactly the
configured value and returns a JSON 413. nginx's `client_max_body_size` is set by
the init script to `MAX_UPLOAD_MIB + 64` MiB. The headroom exists so that an
oversized upload trips the Go limit first and the browser gets a JSON error with a
byte count, rather than nginx's bare HTML 413.

`0` means unlimited, and it means that on both sides: the handler skips the
`MaxBytesReader` and the init script renders `client_max_body_size 0`, which is
how nginx spells "do not check". Nothing then stops a browser from filling the
disk, so only do this when you control who can reach the instance.

Everything else is refused at startup rather than half-applied. A negative value
fails in `config.Load`; a value that is not a whole number of MiB — `4G`, `1.5` —
fails in the init script before nginx is rendered, and the app separately falls
back to the default with a warning. There is no value that makes the two layers
disagree.

Uploads are streamed straight to their final path on disk, so raising this costs
you disk, not RAM.

## PUID / PGID

Both services run as this uid:gid, and everything under `/data` is owned by it.
Set them to your own `id -u` / `id -g` so the files that appear in `./data` on the
host are yours.

The init script does not use `usermod` (busybox does not have it). It looks up
whichever account already owns the id, and only creates one if the id is unused.
That means any value works, including 0.

The startup chown is not recursive, on purpose — walking a large downloads volume
on every boot is a slow way to start a container. So if you change `PUID`/`PGID`
after the first run, the top-level directories get fixed but existing files do
not, and jobs start failing on permission errors. Start once with
`FIX_OWNERSHIP=1`, let the recursive chown finish, then set it back to `0`.

## XACCEL_PREFIX

When this is set (the default), a finished-file download returns an empty body
plus `X-Accel-Redirect: /_protected/<escaped path>` and nginx streams the file off
disk with `sendfile()`. A 10 GB file never passes through the Go process.

Setting it to an empty string makes Go serve the file itself with
`http.ServeFile`. That is correct but slower, and it holds a goroutine and a
socket for the duration of the transfer. It is mainly useful when you are running
the binary outside the image with no nginx in front. This is the one variable
where an empty value is not the same as an absent one: `XACCEL_PREFIX=` really
does turn the handoff off, where every other empty variable falls back to its
default. The container's nginx keeps its default location in that case, unused but
still valid.

Inside the container you do not have to keep the two ends in step yourself. The
init script renders the nginx location from the same two variables the Go process
reads:

```
location $XACCEL_PREFIX/ {
    internal;
    alias $DOWNLOAD_DIR/;
}
```

So moving `DOWNLOAD_DIR` moves the `alias` with it, and renaming the prefix
renames the `location`. The script logs the pair it produced
(`nginx X-Accel location = /_protected/ -> /data/downloads/`) and refuses to boot
if either value is not an absolute path, or contains anything outside letters,
digits and `. _ - /`.

The `internal` line is the load-bearing one and the script always writes it:
without it, anyone could fetch any file under the download directory by guessing
the path. Neither variable is in the `environment:` block of the shipped
`docker-compose.yml`, in keeping with the other path variables — add them there if
you want to change them.

Outside the container the two can still disagree — your own nginx, your own
`alias`. That fails safe: output files that do not resolve under `DOWNLOAD_DIR`
are detected (`accelPath` returns false) and quietly stream through Go instead, so
a mismatch costs you the `sendfile()` fast path rather than serving the wrong
file.

## Reverse proxy: TRUSTED_PROXIES, ALLOWED_HOSTS, PROXY_ONLY

Three variables, each answering a different question. All three default to off,
so a fresh install stays reachable at `http://<lan-ip>:1144`.

### TRUSTED_PROXIES — whose forwarding headers to believe

`X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host` are ordinary
request headers. A client can send whatever it likes in them, so they are only
worth reading when the connection came from a proxy you put there. That set is
loopback (the bundled nginx, always) plus whatever you list here:

```
TRUSTED_PROXIES=172.18.0.0/16
```

Entries are CIDR blocks or bare addresses. With your proxy listed, the app reads
the `X-Forwarded-For` chain from the right and stops at the first hop that is not
one of yours — that is the client. Without it, the nearest hop it can vouch for
is your proxy, and three things go wrong:

- Login rate limiting counts every failed attempt against the proxy, so five
  mistakes lock out everyone.
- The session cookie is issued without `Secure` even though the browser is on
  HTTPS.
- The API believes the browser is on `http://`, so a `POST` from a page loaded
  over `https://` looks cross-origin. Browsers that send `Sec-Fetch-Site` — every
  current one — are waved through on that instead, but anything older gets a 403
  on every job it tries to submit.

The last two are why this matters even when you do not care about the client
address. The bundled nginx is reached over plain HTTP no matter what the browser
is on, so `X-Forwarded-Proto` and `X-Forwarded-Host` are only ever right if they
come from *your* proxy — which is exactly what listing it here permits. The same
value renders an nginx `geo` block (`/etc/nginx/forwarded.conf` in a running
container), so both hops draw the line in the same place: a trusted peer's
`X-Forwarded-Proto`, `X-Forwarded-Host` and `X-Real-IP` are passed through, and
anyone else's are overwritten.

A client that pre-loads `X-Forwarded-For` with forgeries cannot use them to move
the answer: everything a proxy appends lands to the right of whatever the client
sent.

Note that the address to list is the one the container sees. On a bridge network
that is your proxy's address on that network; with the port published and the
proxy on the host it is the Docker gateway (`172.17.0.1` by default), not the
proxy's LAN address.

### ALLOWED_HOSTS — which names the instance answers to

```
ALLOWED_HOSTS=aio.example.com,*.media.example.com
```

Anything else gets 403. Matching is on the hostname only — the port is stripped
first, so entries must not carry one. `*.example.com` matches subdomains at any
depth (`a.example.com`, `a.b.example.com`) but *not* the bare `example.com`;
list it separately if you want it. A lone `*` means any name, which is also what
an empty value means.

### PROXY_ONLY — no direct access by address

```
PROXY_ONLY=1
```

With this set, a request whose `Host` is a bare address is refused:

```
http://192.168.16.35:19882   403
https://aio.example.com      works
```

The reasoning is that a reverse proxy is reached by the name it holds a
certificate for, and nothing else is. So the name is what separates "came
through the proxy" from "found the published port". A request that did not come
from a trusted proxy at all is refused for the same reason, which is what makes
the setting mean something when you run the binary without the bundled nginx.

`localhost` is still allowed — it is only reachable from the box itself, and
blocking it would break `curl` from inside the container for no gain.

Both variables are enforced twice. The Go process applies them to the API, and
the init script renders the same rules into an nginx `map` so the frontend is
covered too — otherwise a blocked name would still load the UI and only fail
once it started making calls. The container healthcheck is exempt: it reaches
`/api/health` over a loopback-only listener on port 8001, because it necessarily
asks with an IP in `Host`.

Neither setting is a substitute for `AUTH_USERNAME`. `Host` is client-supplied;
this stops a browser and a casual scan, not someone who can reach the port and
send whatever header they like. What it does buy you is that the TLS and the
access control living in your proxy cannot be walked around by typing the box's
address instead.
