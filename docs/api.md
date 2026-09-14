# HTTP API

Everything lives under `/api/`. Requests and responses are JSON except for
uploads (multipart), downloads (the file) and the event stream (SSE).

All JSON responses are served as `application/json; charset=utf-8` with a
trailing newline. Errors always use one envelope:

```json
{"error": {"code": "not_ready", "message": "job status is \"queued\", not done"}}
```

Validation failures replace `message` with `field` and `reason` so the UI can
point at the control that is wrong:

```json
{"error": {"code": "invalid_params", "field": "crf", "reason": "must be between 14 and 34 for h264"}}
```

Ordinary request bodies are capped at 1 MiB (`/api/uploads` is exempt and has its
own, much larger limit). Over that you get 413 `body_too_large`.

## Authentication

Session cookie, `aiofiles_session`, HttpOnly, SameSite=Lax, `Secure` only when
the original request was HTTPS (direct TLS or `X-Forwarded-Proto: https`). There
is no API token and no Basic auth. Anything scripted needs a cookie jar.

When `AUTH_USERNAME` is empty the auth middleware is a pass-through and every
endpoint below is open. When it is set, everything except `/api/health` and the
three `/api/auth/*` routes returns 401 without a valid session:

```json
{"error": {"code": "unauthorized", "message": "authentication required"}}
```

Login failures are rate limited per client IP: more than 5 within 15 minutes and
the source gets 429 until the window rolls over. The address is taken from the
`X-Forwarded-For` chain, read from the right and stopping at the first hop that
is not a trusted proxy - loopback plus whatever `TRUSTED_PROXIES` lists - and
falls back to `X-Real-IP` and then the socket. Headers from an untrusted peer
are ignored outright, so anything that can reach the app directly is counted by
its own address and cannot spoof its way around the limit. If your own proxy is
not in `TRUSTED_PROXIES`, every attempt is counted against the proxy instead and
five failures lock out everyone.

If `ALLOWED_HOSTS` or `PROXY_ONLY` is set, a request for a name the instance
does not answer to is refused before any of this, with 403:

```json
{"error": {"code": "host_not_allowed", "message": "this instance does not answer to that address"}}
```

## Endpoints

| Method | Path | Auth | Success |
| --- | --- | --- | --- |
| GET | `/api/health` | no | 200 |
| POST | `/api/auth/login` | no | 200 |
| POST | `/api/auth/logout` | no | 200 |
| GET | `/api/auth/me` | no | 200 |
| GET | `/api/presets` | yes | 200 |
| POST | `/api/probe` | yes | 200 |
| POST | `/api/uploads` | yes | 200 |
| GET | `/api/files` | yes | 200 |
| POST | `/api/jobs` | yes | 202 |
| GET | `/api/jobs` | yes | 200 |
| GET | `/api/jobs/{id}` | yes | 200 |
| POST | `/api/jobs/{id}/cancel` | yes | 202 |
| DELETE | `/api/jobs/{id}` | yes | 200 |
| GET | `/api/jobs/{id}/download` | yes | 200 |
| POST | `/api/jobs/{id}/reuse` | yes | 200 |
| GET | `/api/events` | yes | 200 (SSE) |

Any other path under `/api/` returns a JSON 404 with code `not_found` rather than
net/http's plain-text default.

### GET /api/health

Public, and what the container's `HEALTHCHECK` probes. Because it goes through
nginx it exercises both processes at once.

```json
{"status": "ok", "auth_required": true}
```

### POST /api/auth/login

```json
{"username": "me", "password": "correct-horse-battery-staple"}
```

200 `{"ok":true}` and a `Set-Cookie`. 401 `invalid_credentials`, 429
`too_many_attempts`, 400 `invalid_json`, 500 `internal_error` (which is what a
malformed `AUTH_PASSWORD_HASH` produces - the hash decoder returns an error
rather than a silent "wrong password").

When auth is disabled, login always succeeds and issues nothing.

### POST /api/auth/logout

No body. Always 200 `{"ok":true}`. Drops the session server-side and clears the
cookie.

### GET /api/auth/me

```json
{"authenticated": true, "username": "me", "auth_required": true}
```

`username` is empty unless the request is authenticated. With auth disabled,
`authenticated` is `true` and `auth_required` is `false`. The frontend uses this
to decide whether to show the login page.

### GET /api/presets

Every allow-list the UI renders its pickers from, plus two server limits. Keys:
`video_containers`, `video_codecs`, `encode_speeds`, `resolutions`,
`frame_rates`, `audio_codecs`, `audio_bitrates`, `encode_presets`,
`audio_formats`, `image_formats`, `compress_targets`, `trace_targets`,
`retention_choices`,
`crf_bounds`, `target_size_bounds`, `encode_preset_values`,
`compress_target_values`, `image_compress_quality`, `default_retention_days`,
`max_upload_bytes`.

The option lists are arrays of `{"value","label"}`. `crf_bounds` is
`{"h264":[14,34],…}`. `encode_preset_values` and `compress_target_values` publish
what each intent-level preset actually resolves to, so the UI can show the numbers
instead of hiding them behind a word. Only the `crf` in `compress_target_values`
is what the server will use; its `speed`, `resolution` and `audio_bitrate` are
the values the UI writes into its own controls, and the request may override
them.

This is the authoritative list. Do not hardcode the values below - read them from
here.

### POST /api/probe

```json
{"url": "https://www.youtube.com/watch?v=..."}
```

Runs `yt-dlp -J --no-playlist --no-download` and returns a trimmed view:

```json
{
  "title": "…", "uploader": "…", "duration": 212.0,
  "thumbnail": "https://…", "webpage_url": "https://…",
  "extractor": "Youtube", "is_playlist": false,
  "formats": [
    {"id":"137","ext":"mp4","resolution":"1920x1080","fps":30,
     "vcodec":"avc1.640028","acodec":"none","filesize":41234567,
     "bitrate":2500.0,"note":"1080p","has_video":true,"has_audio":false}
  ]
}
```

Formats come back sorted: video first (tallest, then highest bitrate), then
audio-only by bitrate. `filesize` is 0 when the extractor does not know it.

Only `http` and `https` URLs are accepted, max 2048 characters, no leading dash.
Bad URL → 400. No prober wired up → 503 `probe_unavailable`. Over 30 seconds →
504 `probe_timeout`. yt-dlp failed → 502 `probe_failed` with a generic message;
its stderr goes to the server log only, since it quotes what the extractor
fetched.

Each probe forks a yt-dlp process outside the job queue, so the endpoint carries
its own limit: at most `MAX_CONCURRENT_JOBS` (capped at 4) in flight and a burst
of 8 refilling one every 3 seconds, counted instance-wide. Over that → 429
`probe_busy` with `Retry-After`.

A playlist URL is described through its first entry and `is_playlist` is true.
Downloading a whole playlist is not supported - the download runner always passes
`--no-playlist`.

### POST /api/uploads

`multipart/form-data` with one part named `file`. Other parts are skipped; the
first file part wins and the rest of the body is ignored.

```json
{"upload_id": "3f2a1c9d8e7b6a50-holiday.mov", "filename": "holiday.mov", "size": 734003200}
```

The `upload_id` is the stored basename: 16 hex characters, a dash, then the
sanitised original name. Hand it back in `POST /api/jobs`. It is not a database
row - the mapping is the filename itself.

Filenames are sanitised on the way in: separators, whitespace and shell-hostile
characters become `_`, control characters are dropped, non-UTF-8 is repaired, and
the name is truncated to 120 runes with the extension preserved. Unicode
otherwise survives. A name that sanitises to nothing becomes `upload`.

400 `invalid_multipart` for a non-multipart body, 400 `missing_file` if no `file`
part was found, 413 `file_too_large` past `MAX_UPLOAD_MIB`, 500 `upload_failed`
for a write error.

Uploads are not garbage collected on their own. They are removed when the job
that consumed them is deleted or swept by retention. An upload that never becomes
a job stays on disk.

### GET /api/files

Lists the files this instance still holds that can be fed back into a new job:
the output of every finished job and every source upload still on disk. Files
shared by several jobs appear once.

```json
{"files": [
  {"job_id": "9f1c…", "source": "output", "name": "holiday.mkv", "size": 512000, "created_at": "2026-08-18T12:57:21.442Z"},
  {"job_id": "9f1c…", "source": "input", "name": "holiday.mov", "size": 734003200, "created_at": "2026-08-18T12:57:21.442Z"}
]}
```

### POST /api/jobs/{id}/reuse

Turns one of those files into a fresh upload without a second transfer, so the
same bytes can be converted or compressed again.

```json
{"source": "output"}
```

The reply is the `POST /api/uploads` reply: `{"upload_id", "filename", "size"}`.
The file is hard-linked into `UPLOAD_DIR` when both directories share a
filesystem and copied otherwise, so deleting the original job leaves the new
upload intact.

404 `not_found` for an unknown job, 404 `file_missing` for an unknown `source`
or a file that is no longer on disk, 500 `upload_failed` for a write error.

### POST /api/jobs

```json
{
  "type": "download",
  "url": "https://www.youtube.com/watch?v=...",
  "params": {"mode": "video", "format_id": "137", "container": "mp4", "retention_days": 7}
}
```

`type` is `download`, `convert`, `compress`, `image` or `edit`. A `download` job
needs `url`; the others need `upload_id` instead. `params` is validated against
the type-specific struct with unknown fields rejected - a typo'd key is a 400,
not a silently ignored option.

Returns 202 and the created job. It is queued, not started; watch `/api/events`.

Params by type. Every string value must appear in the matching `/api/presets`
list.

`download`: `mode` (`video`|`audio`, default `video`), `format_id` (a plain
yt-dlp format id or `a+b` merge, `[A-Za-z0-9_.-]` only, max 64 chars; empty means
"best"), `audio_format` and `audio_bitrate` (audio mode), `container` (video mode,
used as `--merge-output-format`), `embed_subs`, `embed_thumbnail`,
`embed_metadata`, `retention_days`.

`convert`: `preset` (default `custom`), `container` (`mp4`), `video_codec`
(`h264`), `speed` (`medium`), `crf` (`23`), `resolution` (`source`), `frame_rate`
(`source`), `audio_codec` (`aac`), `audio_bitrate` (`192`), `strip_metadata`,
`retention_days`. A non-`custom` preset overrides `speed` and `crf` - and the
stored params record the resolved values, so the job record matches what actually
ran.

`compress`: `target` (default `balanced`), `target_size_mb` (only with
`target: "target_size"`, 1–65536), `container` (`mp4`), `video_codec` (`h264`),
`speed` (`medium`), `resolution` (`source`), `audio_bitrate` (`128`),
`strip_metadata`, `retention_days`. Audio is always re-encoded to AAC and the
frame rate is always left alone; there are no parameters for either.

`image`: `format` (default `webp`; `source` keeps the input format; `svg` writes
an SVG that embeds the render; `svg_trace` runs potrace and writes real paths,
black and white only; both ignore `quality` and take their size from
`width`/`height`), `trace` (`dark`|`light`, default `dark`, `svg_trace` only:
which side of the threshold becomes paths), `quality`
(1–100, default 82), `width`, `height` (0 keeps that dimension, max 20000),
`strip_metadata` (default true), `retention_days`.

`edit`: `start` and `end` in seconds (`end: 0` runs to the end of the file, and
the cut must keep at least a tenth of a second), `crop_x`, `crop_y`, `crop_w`,
`crop_h` in source pixels (`crop_w: 0` keeps the whole frame; width and height
are set together, rounded down to an even number and at least 16),
`retention_days`. The cut lands exactly where it was asked for, which a stream
copy cannot do - it can only start on a keyframe - so video is re-encoded to
H.264 at CRF 18 while the audio is copied, landing in MP4 or, where H.264 does
not belong, MKV. A file with no video track (mp3, m4a, flac, ...) is copied
whole and keeps its own container.

Validation is not only per field. Container/codec combinations ffmpeg would
refuse to mux are rejected up front - WebM only takes VP9 or AV1 with Opus, MP4
and MOV reject VP9 and FLAC. `video_codec: "copy"` with a non-custom preset is
rejected because there is nothing to tune, and with `target_size` because a
copied stream keeps its original bitrate.

400 `invalid_params` with the offending field. 503 `queue_full` when
`QUEUE_DEPTH` submissions are already waiting - note the job row is still created
and marked failed, so the rejection is visible in the UI rather than vanishing.
503 `no_runner` if no runner is registered for the type (should not happen in the
shipped binary).

### GET /api/jobs

Query parameters: `status` (exact match on one of the status values) and `limit`.
`limit` outside 1–500 is clamped to 200. A negative or non-numeric `limit` is a
400.

```json
{"jobs": [ … ]}
```

Newest first. Always an array, never `null`.

A job looks like this:

```json
{
  "id": "9f1c0b7a4e2d5c8b6a3f1e0d",
  "type": "convert",
  "title": "holiday.mov",
  "source": "holiday.mov",
  "params": {"container": "mp4", "video_codec": "h264", "…": "…"},
  "output_name": "holiday-9f1c0b7a.mp4",
  "output_size": 20971520,
  "status": "done",
  "progress": 100,
  "stage": "done",
  "created_at": "2026-08-02T10:15:00Z",
  "updated_at": "2026-08-02T10:17:42Z",
  "started_at": "2026-08-02T10:15:01Z",
  "finished_at": "2026-08-02T10:17:42Z",
  "expires_at": "2026-08-09T10:15:00Z"
}
```

`error` is present only when non-empty. The three timestamp pointers are omitted
when null. `expires_at` is absent for a "forever" job. Absolute paths on disk -
input and output - are deliberately not in the JSON.

`status` is one of `queued`, `running`, `done`, `failed`, `canceled`, `expired`.
In practice you will never see `expired`: the sweeper deletes the row rather than
relabelling it. The status exists in the model but nothing sets it.

`source` is the URL for downloads and the original filename otherwise. `title`
starts as the same thing and is replaced with yt-dlp's real title once a download
finishes.

### GET /api/jobs/{id}

The same object, or 404 `not_found`.

### POST /api/jobs/{id}/cancel

Kills the running job's process group. 202 `{"ok":true,"id":"…"}`.

409 `not_cancelable` if the job is not *running* - including when it is still
queued. There is no way to remove a job from the queue; delete it instead, and
the worker will pick up a row that no longer exists and log the failure.

The job ends up in status `canceled` and any partial output file is removed.

### DELETE /api/jobs/{id}

Cancels the job if it is running, deletes the output file and the source upload,
then deletes the row. 200 `{"ok":true,"id":"…"}` or 404. Publishes a `deleted`
event.

### GET /api/jobs/{id}/download

Serves the finished file with a `Content-Disposition: attachment` header (RFC
5987 `filename*` form when the name is not ASCII).

With `XACCEL_PREFIX` set - the default in the container - the response is an empty
200 carrying `X-Accel-Redirect`, and nginx does the actual sending. Without it,
or when the output is somehow outside `DOWNLOAD_DIR`, Go serves the file directly
and you get range support from `http.ServeFile`.

409 `not_ready` if the job is not `done`. 404 `no_output` if the job produced no
file, 404 `file_missing` if the file has been swept or deleted underneath it.

### GET /api/events

Server-sent events. One stream carries every job's activity; there is no
per-job subscription and no way to filter.

Response headers include `Content-Type: text/event-stream`, `Cache-Control:
no-cache` and `X-Accel-Buffering: no`. The bundled nginx also turns off
`proxy_buffering`, `proxy_cache` and `chunked_transfer_encoding` for this exact
location - without that the stream sits in a buffer and progress never arrives.

Events:

| Event | When | Data |
| --- | --- | --- |
| `ready` | immediately on connect | `{}` |
| `created` | a job is submitted | `{"kind","job_id","job"}` |
| `progress` | a runner reports progress | `{"kind","job_id","progress"}` |
| `status` | a job changes state | `{"kind","job_id","job"}` |
| `deleted` | a job is deleted or swept | `{"kind","job_id"}` |

`progress` is `{"percent": 0–100, "stage": "downloading", "speed": "1.2MiB/s",
"eta": "00:42"}`. `speed` and `eta` are omitted when unknown. `percent` is
**negative when the percentage is genuinely unknown** - a stage change, or an
ffmpeg run on a file whose duration ffprobe could not read. Treat a negative
value as "show an indeterminate bar", not as 0. Typical stages are `probing`,
`downloading`, `merging`, `converting`, `encoding`, `analyzing`, `done`.

A `: keepalive` comment line goes out every 25 seconds, comfortably under nginx's
60-second default idle timeout.

Two things to design your client around. Progress events are throttled to roughly
one every 400 ms per job, so do not expect every ffmpeg tick. And the bus drops
events for a subscriber that is not keeping up rather than blocking a worker - the
frontend handles this by re-fetching `/api/jobs` on every (re)connect instead of
assuming the stream is complete. Do the same.

## curl

Log in and keep the cookie:

```sh
curl -sS -c jar.txt -X POST http://localhost:1144/api/auth/login \
  -H 'content-type: application/json' \
  -d '{"username":"me","password":"correct-horse-battery-staple"}'
```

Probe a URL, then start a download of format 137 merged to MP4:

```sh
curl -sS -b jar.txt -X POST http://localhost:1144/api/probe \
  -H 'content-type: application/json' \
  -d '{"url":"https://www.youtube.com/watch?v=dQw4w9WgXcQ"}'

curl -sS -b jar.txt -X POST http://localhost:1144/api/jobs \
  -H 'content-type: application/json' \
  -d '{"type":"download","url":"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
       "params":{"mode":"video","format_id":"137","container":"mp4","retention_days":7}}'
```

Upload a file and compress it to roughly 200 MB:

```sh
UPLOAD=$(curl -sS -b jar.txt -F file=@holiday.mov http://localhost:1144/api/uploads \
         | sed -n 's/.*"upload_id":"\([^"]*\)".*/\1/p')

curl -sS -b jar.txt -X POST http://localhost:1144/api/jobs \
  -H 'content-type: application/json' \
  -d "{\"type\":\"compress\",\"upload_id\":\"$UPLOAD\",
       \"params\":{\"target\":\"target_size\",\"target_size_mb\":200,\"video_codec\":\"h265\"}}"
```

Watch the stream (`-N` disables curl's own buffering, which otherwise hides the
point of the exercise):

```sh
curl -sS -N -b jar.txt http://localhost:1144/api/events
```

Fetch the result under its own name:

```sh
curl -sS -b jar.txt -OJ http://localhost:1144/api/jobs/9f1c0b7a4e2d5c8b6a3f1e0d/download
```
