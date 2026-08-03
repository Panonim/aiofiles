# AIOFiles documentation

AIOFiles is a self-hosted web application for downloading media, converting and
compressing files, and processing images. The application is packaged as a
Docker image with a Go API, nginx frontend, background workers, SQLite storage,
`yt-dlp`, `ffmpeg`, and ImageMagick.

## Start here

For a new installation, follow the [project README](../README.md). It covers
the Docker Compose quick start, the default port, authentication setup, and the
main features.

The standard deployment is:

1. Copy `.env.example` to `.env` and adjust the values for your host.
2. Start the container with `docker compose up -d`.
3. Open `http://localhost:1144`, or use the hostname configured for your reverse
   proxy.
4. Keep `./data` backed up. It contains the database, uploads, temporary files,
   and completed downloads.

## Documentation

- [Configuration](configuration.md) - environment variables, defaults,
  validation rules, authentication, storage, and reverse-proxy settings.
- [HTTP API](api.md) - authentication, endpoints, request and response bodies,
  job states, Server-Sent Events, and runnable `curl` examples.
- [Troubleshooting](troubleshooting.md) - common startup, authentication,
  upload, proxy, download, and media-processing failures.

## Operational notes

AIOFiles runs jobs in the background. Downloads and conversions are submitted
to a bounded queue, workers report progress through the web UI and the API's
SSE endpoint, and finished output is retained according to the configured job
retention period.

Authentication is optional. If it is enabled, configure both
`AUTH_USERNAME` and `AUTH_PASSWORD_HASH`; the password hash must be single
quoted in `.env` because it contains `$` characters. The safer production
setup is to place AIOFiles behind HTTPS and configure `TRUSTED_PROXIES`,
`ALLOWED_HOSTS`, and `PROXY_ONLY` as appropriate.

The container's writable state is divided into these directories:

| Directory | Contents |
| --- | --- |
| `./data/downloads` | Completed job output |
| `./data/uploads` | Files uploaded for processing |
| `./data/db` | SQLite database |
| `./data/tmp` | Temporary files for active jobs |

See [configuration.md](configuration.md) before changing paths, worker limits,
upload limits, retention, or proxy behavior. See [troubleshooting.md](troubleshooting.md)
when a container starts but a particular job or request fails.
