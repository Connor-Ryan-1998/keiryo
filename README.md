# keiryō — lightweight monitoring

A Go hub and Rust agents for host/Docker metrics and on-demand container logs.
Embedded dashboard, SQLite history and durable agent queues; metrics use OTLP/HTTP.
No Ansible or infrastructure repository required.

## Requirements

- macOS or Linux; production collection targets Linux.
- Go matching [go.mod](go.mod).
- Rust/Cargo, rustfmt and Clippy matching [rust-toolchain.toml](agent-rust/rust-toolchain.toml).
- [Go Task](https://taskfile.dev/) and [uv](https://docs.astral.sh/uv/); uv provisions Python 3.12.
- Optional: Docker for container metrics; Node.js 18+ for UI tests.

Commands below use `task`. Substitute `go-task` if that is your installed binary.

## Quickstart

```sh
git clone https://github.com/Connor-Ryan-1998/keiryo.git
cd keiryo
uv sync --locked
uv run --locked task preview
```

Already checked out? Run the last two commands from the application root.

1. Open the printed `Preview: http://127.0.0.1:<port>` URL.
2. Sign in with **admin / admin**, set a preview-only password, then sign in again.
3. Wait about one minute for CPU/rate readings.

Preview monitors your real machine, binds to loopback and changes no deployed services.
**Ctrl+C** stops both processes and deletes temporary credentials/history.
After code or UI changes, rerun the preview command to rebuild; use its new URL.
For retained history, follow [persistent setup](docs/reference.md#persistent-local-setup).

## Development

Run from the application root:

```sh
uv run --locked task build TARGET=native  # Build hub + agent into bin/
uv run --locked task test                 # Rust, Go race and preview tests
uv run --locked task test SUITE=ui         # Chart logic; requires Node.js
uv run --locked task lint                 # Rust formatting + Clippy
uv run --locked task format COMPONENT=go
uv run --locked task format COMPONENT=agent
```

Keep `bin/monitoring` and `bin/monitoring-agent` together; the first dispatches agent commands to its Rust sibling.
UI assets are embedded: rebuild to load changes.

### Container build

```sh
docker build -t keiryo:dev .
```

Builds a Linux image containing both executables. It does not start services,
publish the image or supply deployment configuration.

### Releases

Successful `main` builds publish multi-architecture images to
`ghcr.io/connor-ryan-1998/keiryo`. CI assigns patch versions starting at
`0.0.1`, creates matching Git tags (`v0.0.1`) and GitHub Releases with
generated release notes, and promotes the same image to `latest`.
Commit-specific `sha-<commit>` images remain available for traceability.
Pull requests only test and build; they do not publish.

`VERSION` sets the manually managed release series. Keep its patch field at
zero: change `0.0.0` to `0.1.0` for a new minor series or `1.0.0` for a new
major series. CI publishes `0.1.1` or `1.0.1` next, respectively. Do not
decrease the series or manually increment the patch field.

Main workflow runs are serialized without automatic cancellation. Superseded
commits skip release publication. Rerunning a completed release does not move
its numbered image or `latest`. A version is reserved with a Git tag after
the image build succeeds, before registry promotion. If publication fails,
rerun the failed release job while that commit is still main HEAD. Never
delete or move reserved tags: a newer commit gets a new patch, so failed
publications can leave gaps. Avoid rerunning successful container jobs;
rerun failed jobs only to preserve the original SHA image.

Repository rules must allow the workflow token to create `v*` tags and
GitHub Releases and write the GHCR package. Releases contain generated
source archives and image coordinates, not standalone binary downloads.
Run release regression tests with `uv run --locked task test SUITE=release`.

## Configuration and safety

- Start with [hub](examples/hub.json) and [agent](examples/agent.json) JSON examples.
- Each host needs its own token file. Never commit credentials or databases.
- Keep preview/bootstrap private; never expose it through a public tunnel.
- Docker socket access is effectively host-root access, even with a read-only mount.
- Remote traffic requires HTTPS or a private encrypted network; the app has no built-in TLS.
- Back up before upgrading. Upgrade the hub before agents; rollback may require an older database backup.
- Missing readings are unavailable, not zero. This is not a general-purpose OTLP backend.

## Reference

- [Configuration fields and alert policy](docs/reference.md#configuration)
- [Optional Discord notifications for all alerts](docs/reference.md#discord-notifications)
- [Backup freshness and failure monitoring](docs/reference.md#expected-backup-jobs)
- [OTLP, durable delivery and upgrades](docs/reference.md#opentelemetry-metrics)
- [Authentication and proxy requirements](docs/reference.md#authentication-and-network-boundary)
- [Storage, backups and limits](docs/reference.md#storage-backups-and-limits)
- [Advanced tests and builds](docs/reference.md#verification)
- [Troubleshooting](docs/reference.md#preview-and-troubleshooting)
- [Third-party notices](THIRD_PARTY.md)
