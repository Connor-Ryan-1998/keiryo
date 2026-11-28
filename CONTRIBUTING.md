# Contributing to keiryō

Thanks for your interest! Bug reports, docs fixes and focused pull requests are all welcome.
For larger changes, open an issue or discussion first so we can agree on the approach.

## Requirements

- macOS or Linux; production collection targets Linux.
- Go matching [go.mod](go.mod).
- Rust/Cargo, rustfmt and Clippy matching [rust-toolchain.toml](agent-rust/rust-toolchain.toml).
- [Go Task](https://taskfile.dev/) and [uv](https://docs.astral.sh/uv/); uv provisions Python 3.12.
- Optional: Docker for container metrics and integration tests; Node.js 18+ for UI tests.

Commands below use `task`. Substitute `go-task` if that is your installed binary.

## Local preview

```sh
uv sync --locked
uv run --locked task preview
```

1. Open the printed `Preview: http://127.0.0.1:<port>` URL.
2. Sign in with **admin / admin**, set a preview-only password, then sign in again.
3. Wait about one minute for CPU/rate readings.

Preview monitors your real machine, binds to loopback and changes no deployed services.
**Ctrl+C** stops both processes and deletes temporary credentials/history.
After code or UI changes, rerun the preview command to rebuild; use its new URL.
For retained history, follow [persistent setup](docs/reference.md#persistent-local-setup).

## Development

Run from the repository root:

```sh
uv run --locked task build TARGET=native  # Build hub + agent into bin/
uv run --locked task test                 # Rust, Go race and preview tests
uv run --locked task test SUITE=ui         # Chart logic; requires Node.js
uv run --locked task lint                 # Rust formatting + Clippy
uv run --locked task format COMPONENT=go
uv run --locked task format COMPONENT=agent
uv run --locked task screenshots          # Regenerate docs/images (Playwright)
```

Keep `bin/monitoring` and `bin/monitoring-agent` together; the first dispatches agent commands to its Rust sibling.
UI assets are embedded: rebuild to load changes. More suites (browser, integration, recovery, OTLP)
are listed under [verification](docs/reference.md#verification).

`task screenshots` needs a Playwright Chromium install
(`uv run --locked --with playwright==1.57.0 python -m playwright install chromium`)
or `MONITORING_CHROME` pointing at an existing Chromium/Chrome. It seeds a synthetic
fleet into a temporary hub; it never reads your real machine.

### Container build

```sh
docker build -t keiryo:dev .
```

Builds a Linux image containing both executables. It does not start services,
publish the image or supply deployment configuration.

## Pull requests

- Keep changes focused and include tests for behaviour changes.
- Run `task lint` and `task test` before pushing; CI runs the same plus UI and recovery suites.
- Never commit credentials, tokens, databases or local configuration.
- Use [Conventional Commits](https://www.conventionalcommits.org/) style messages (`feat:`, `fix:`, `docs:`).
- Releases are automated from `main`; see [docs/releasing.md](docs/releasing.md).

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
