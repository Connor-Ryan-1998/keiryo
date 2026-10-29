# keiryō — lightweight monitoring · V1

A Go hub and Rust agents for generic host/Docker metrics and on-demand container logs. Embedded HTML/HTMX, hub-side SQLite and an agent-local redb metrics queue. No Ansible, inventory, Traefik or Tailscale dependency is required to build or run the application.

Metrics use **OTLP/HTTP**. The hub accepts the supported host/Docker metric mapping from our agent or a stock OpenTelemetry Collector; private agent state and on-demand log fetching remain keiryō-specific. This is not a general-purpose backend for arbitrary OTLP metrics, logs or traces.

**keiryō (軽量)** means “lightweight.” The blue feather with a pulse-shaped shaft is the app icon. Its transparent SVG master is [`internal/hub/static/keiryo.svg`](internal/hub/static/keiryo.svg), with transparent PNG exports at 32 and 512 pixels alongside it. The header and favicon use the SVG; the 32-pixel PNG is the favicon fallback. Binary names and configuration remain `monitoring`.

## Try keiryō on your own machine

The quickest way to try the dashboard is the interactive preview. It builds and runs a **real local hub and agent**, collecting your machine's metrics rather than synthetic samples. No Ansible, vault credentials, cloud account or deployment is needed. It does not build or restart Docker containers, publish images, or change your running services.

### Prerequisites

- **macOS or Linux**, with Go matching [`go.mod`](go.mod) and [Go Task](https://taskfile.dev/) (`task`, sometimes packaged as `go-task`).
- **Rust**, matching [`agent-rust/rust-toolchain.toml`](agent-rust/rust-toolchain.toml), including Cargo, rustfmt and Clippy. Keep built `monitoring` and `monitoring-agent` executables together; `monitoring agent` dispatches to its Rust sibling without falling back to Go.
- [uv](https://docs.astral.sh/uv/) and Python **3.12** for the preview and test helpers; uv can provision Python.
- **Optional:** a running local Docker daemon and permission to access its Unix socket, for container metrics and on-demand logs. Host metrics work without Docker.
- **For automated UI tests:** Node.js **18+**; browser tests also need Chrome/Chromium or Playwright's Chromium installation. Neither is needed just to open the preview in your own browser.

### Start the interactive preview

Clone the standalone repository and prepare its Python environment:

```sh
git clone https://github.com/Connor-Ryan-1998/keiryo.git
cd keiryo
uv sync --locked
```

Then run from the repository root:

```sh
uv run --locked task preview
```

The preview uses only Python's standard library. No infrastructure repository,
Ansible environment or deployment credentials are required.

1. Keep the terminal running. Find its **`Preview: http://127.0.0.1:<port>`** line and open that exact URL in your browser; the port is chosen automatically, not fixed at 9080.
2. Sign in with **admin / admin**, set a preview-only password (6–72 bytes, including punctuation or a symbol), then sign in again. Do not reuse a real password.
3. Check **Fleet Overview**, **Resource Views**, **Fleet Metrics**, and **Alerts & Hosts**. Open the local host for detailed charts and container observations. Logs are fetched only when you request them.
4. Allow about **one minute** for the second sample: CPU and counter rates need two observations. History fills as you leave the preview running; missing measurements are unavailable, not zero. One host cannot demonstrate a large fleet or every threshold state.

Both listeners bind to `127.0.0.1`. Credentials, configuration and SQLite history live in the private directory printed as **`Preview data:`**, separate from examples and production data. Do not share that directory or expose the preview through a public tunnel.

### Stop or reload after code changes

Press **Ctrl+C** in the preview terminal. This stops the agent and hub and removes their temporary credentials and history. Every new preview starts fresh, including its admin password and URL.

After changing Go code, templates, CSS, JavaScript or icons, stop the preview and rerun the same preview command, then open the newly printed URL. **The UI assets are embedded in the binary:** reloading the browser or restarting an old binary alone does not load source changes. The preview task rebuilds automatically. Hard-refresh the browser if an old favicon remains cached. Use the manual setup below if you need history and login state to survive restarts.

### Docker and platform behaviour

Docker socket selection follows `DOCKER_CONTEXT`, then `DOCKER_HOST`, then the current Docker context, with a local socket fallback if the CLI is unavailable. Only Unix sockets are allowed; SSH/TCP contexts fail rather than silently contacting a remote daemon. To inspect your selection, run `docker context show` and `docker context inspect`. Clear unwanted `DOCKER_CONTEXT`/`DOCKER_HOST` overrides or select a trusted local context before starting the preview.

With Docker Desktop on macOS, host readings describe **macOS**, while containers run in Docker's Linux VM; those readings need not match the VM's resource totals. The preview excludes macOS's virtual `devfs` filesystem, whose 100%-full reading is not a disk-capacity condition. Native macOS previewing is supported, but production collection targets Linux and still needs a Linux trial.

**Docker socket access is privileged in effect**, even though this agent only implements read operations. Use a daemon you trust. Do not make its socket world-writable or expose the Docker API to fix permissions.

### Troubleshooting

| Symptom | What to check |
| --- | --- |
| `task`, `go` or `uv` not found | Install the prerequisites and make sure they are on `PATH`. If Go Task is named `go-task`, substitute that command. |
| uv cannot find a project/lockfile | In this checkout, run `uv sync --locked --group dev` at the repository root. For an extracted app directory, use the standalone `--no-project` preview command above. |
| Browser cannot connect | Keep the preview terminal running and use the URL from the current run. Check terminal errors; the preview uses a new port each time. |
| No CPU/rate values or sparse charts | Wait for the second sample (about a minute) and leave the preview running to accumulate history. Check host collection errors; do not interpret gaps as zero. |
| No containers or Docker collection errors | Start your local daemon, inspect the selected Docker context/socket and check your socket permissions. Host metrics should still arrive without Docker. |
| SSH/TCP Docker context rejected | Select a local Unix-socket context; remote daemon access is deliberately unsupported by the preview. |
| New UI or icon does not appear | Stop and rerun the preview to rebuild, open the new URL, then hard-refresh. |
| Password/history disappeared | Expected after stopping the temporary preview. Use the persistent manual setup below instead. |

### Automated tests versus interactive preview

The preview is for browsing **your own system** and remains running until stopped. Automated tests terminate on completion. From the application directory in this checkout:

```sh
uv run --locked task test  # Go unit/race tests and real preview lifecycle tests
uv run --locked task test SUITE=ui    # chart logic tests; Node.js 18+
```

For an isolated **synthetic fleet** and headless desktop/mobile browser checks, install Playwright Chromium once, then run:

```sh
uv run --locked --with playwright==1.57.0 python -m playwright install chromium
uv run --locked task test SUITE=browser
```

Alternatively, point `MONITORING_CHROME` at an existing Chrome/Chromium executable. For example, on macOS:

```sh
MONITORING_CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
MONITORING_SCREENSHOT="$HOME/Desktop/monitoring-preview.png" \
uv run --locked task test SUITE=browser
```

The browser test leaves no interactive server running and never connects to your deployed hub. In an extracted checkout without a uv project, use `uv run --no-project --python 3.12 task test` and `uv run --no-project --python 3.12 task test SUITE=ui`. For browser checks there, run `uv run --no-project --python 3.12 task build TARGET=native`, then invoke the helper directly with `uv run --no-project --python 3.12 --with playwright==1.57.0 python tests/ui/browser.py` (install Chromium with the same uv options plus `python -m playwright install chromium`, or set `MONITORING_CHROME`).

### Manual local setup with persistent history

Use this instead of the temporary preview when you want fixed ports, custom JSON configuration or history that survives a restart. From the repository root, build the native binary and create the example token **once**:

```sh
uv run --locked task build TARGET=native
(umask 077; set -C; openssl rand -hex 32 > examples/node-1.token)
bin/monitoring hub -config examples/hub.json
```

The token command refuses to overwrite an existing file; keep the existing token on subsequent runs. Open `http://127.0.0.1:9080`, sign in with **admin / admin**, and set a private password. The example stores SQLite data in `data/monitoring.db`; a normal restart preserves the password and history. Keep initial setup private: another person who can reach bootstrap can claim the default account first. All sessions are revoked after a password change.

In a second terminal on the same Linux host, run the agent with the corresponding config/token:

```sh
bin/monitoring agent -config examples/agent.json
```

Stop each process with **Ctrl+C**. To load source changes, stop both, rebuild and restart; to change JSON configuration, restart the affected process. Do not delete the database to update the UI. On macOS, prefer the interactive preview for automatic Docker Desktop socket selection and the `devfs` exclusion; manual configuration needs those adjustments explicitly.

For other hosts, copy `examples/agent.json` and adjust its stable `host_id`, display name, hub URL and credential path. Give each host its **own** random token; add the same token's file path to the hub's `agents` mapping. Files must be regular files with permissions `0600` or stricter, containing at least 32 non-whitespace characters. Tokens/configuration are not compiled into the executable. Never commit real tokens, local configuration or databases.

The example agent URL is loopback for development. Remote HTTP ingestion is only appropriate over a private encrypted overlay; use HTTPS on other networks. There is no built-in TLS server or automatic agent registration.

## Configuration

JSON files override defaults; restart to apply. Unknown keys, multiple JSON values and invalid limits fail startup. Relative database/socket/credential paths resolve against the JSON file's directory. Do not put secrets in JSON examples.

### Hub fields

| Field | Default / meaning |
| --- | --- |
| `ui_listen` | `127.0.0.1:9080` |
| `agent_listen` | `127.0.0.1:9081`; separate private listener |
| `database` | `monitoring.db` |
| `auth_mode` | `local` or explicit `proxy`; never automatic fallback |
| `public_url` | `http://127.0.0.1:9080`; HTTPS except numeric loopback development |
| `trusted_proxies` | Empty; required for proxy mode, CIDR strings |
| `agents` | Host ID → credential-file path |
| `raw_hours` | `24` |
| `bucket_seconds` | `300`; grid is recorded in the database |
| `summary_days` | `30` |
| `storage_bytes` | `1073741824`; retained-page target, **not** total file-size cap |
| `stale_seconds`, `offline_seconds` | `180`, `300` since accepted contact |
| `disk_health_mountpoints` | `[]` (all mounts); exact clean absolute mountpoints for disk health, alerts and the fleet storage table, e.g. `["/"]`. Collection and detailed host charts are unchanged. |
| `alert_policy` | Resource thresholds and container severity/lifecycle policy; see below. Omitted fields retain defaults. |
| `future_seconds` | `300`; maximum future sample clock skew |
| `max_body_bytes` | `2097152`; agent request limit |
| `max_entities` | `2048` metrics/containers per sample |
| `max_requests` | `32` concurrent requests per UI/API boundary |
| `log_lines`, `log_bytes` | `200`, `262144` per snapshot |
| `log_timeout_seconds`, `log_queue` | `15`, `32`; one pending request per host |

Changing retention is supported. Changing the summary bucket grid requires a new database rather than silently mixing incompatible summaries. Existing sessions and login state live in SQLite; deleting the database destroys them and the metrics history.

### Alert policy

The hub receives observations from agents and evaluates a policy supplied through its JSON configuration. No service names or deployment-specific exceptions are built into the application. This controls dashboard conditions, aggregate host health, meters, container badges and chart threshold markers/colours—not notifications or persisted alerts.

Example hub configuration fragment (replace the example host/container names):

```json
{
  "alert_policy": {
    "resources": {
      "cpu": {"warning": 80, "critical": 95},
      "memory": {"warning": 80, "critical": 90},
      "disk": {"warning": 80, "critical": 90}
    },
    "containers": {
      "stopped": "warning",
      "unhealthy": "critical",
      "starting": "warning"
    },
    "container_overrides": [
      {"host": "node-1", "name": "database-migration", "lifecycle": "oneshot", "failure": "critical"}
    ]
  }
}
```

- **Defaults:** each resource warns at **≥80%** and becomes critical at **≥90%**. All three container severities default to `warning`. Partial configuration retains unspecified defaults, including individual thresholds. Resources require finite `0 <= warning < critical <= 100`; severity values must be `warning` or `critical`. Unknown fields and invalid rules fail startup.
- **Container rules:** `stopped` applies to any state other than `running`; running containers use `unhealthy` or `starting` when Docker reports that health status. Running containers without a health check have no container condition. Collection errors remain warnings; missing/stale readings remain unavailable independently of policy.
- **One-shot precedence:** an override matches an exact stable **host ID**, not its display name, and exact container name (without Docker's leading `/`). Both are required; duplicate host/name rules are rejected, and at most 2,048 rules are allowed. Only `oneshot` lifecycle is supported. A matching `exited` container with a known exit code of **0** is healthy; a nonzero exit uses `failure`, which defaults to the configured `stopped` severity. The override does not suppress unhealthy running containers, unstarted jobs, or unknown exit codes. Other hosts/containers use the global rules. An absent/deleted container is not detected as a missing expected service.
- **Observations stay intact:** policy changes do not delete containers, change application data, or rewrite stored history. Charts use the current policy, including for historical readings. Exit codes are optional observations; old samples and failed/missing inspection fields remain unknown, never assumed successful.
- **Upgrade hub first, then agents.** Older hubs reject the new `exit_code` sample field. The upgraded hub accepts older agents and existing SQLite data without a migration; one-shot successes remain warnings until an upgraded agent supplies a known successful exit code. Restart the hub to apply policy changes; there is no live reload. For rollback, downgrade agents before the hub and remove unsupported configuration fields.

### Agent fields

| Field | Default / meaning |
| --- | --- |
| `host_id` | Required stable identifier, 1–128 letters/digits/`_.-` (starts alphanumeric) |
| `display_name` | Optional display label |
| `hub_url` | Required HTTP(S) origin, private agent listener |
| `otlp_metrics_url` | Optional full HTTP(S) metrics URL; defaults to `hub_url` + `/v1/metrics`. Private state/log requests still use `hub_url`. No URL credentials, query or fragment. |
| `credential_file` | Required separate token file |
| `interval_seconds` | `60` |
| `buffer_samples` | Legacy validated field, accepted for compatibility; metrics use durable time/byte bounds and state is latest-only |
| `queue_database` | `agent.redb`, relative to the configuration directory; keep on persistent local storage |
| `queue_retention_seconds` | `7200`, two hours pending retention for a one-hour connectivity outage plus recovery margin |
| `queue_max_bytes` | `268435456` (256 MiB), queued protobuf payload limit, **not** a physical database-file limit |
| `poll_seconds` | `5`, independent of metric collection |
| `docker_socket` | `/var/run/docker.sock` |
| `exclude_interfaces` | `["lo"]`; exact names |
| `exclude_filesystems` | `[]`; exact mountpoints or filesystem types; pseudo-filesystems already excluded by collector |
| `log_lines`, `log_bytes`, `log_timeout_seconds` | `200`, `262144`, `15`; agent-side upper bounds |

No remote configuration or live reload. Deployment tooling may override these same fields without changing application defaults.

## OpenTelemetry metrics

The private agent listener accepts **`POST /v1/metrics`**, using `application/x-protobuf` or protobuf JSON (`application/json`), with optional gzip compression. It stays on the configured `agent_listen` port (9081 by default), not a newly exposed 4318 listener. Success is HTTP 200 with an OTLP metrics response. Unsupported/invalid points are reported using OTLP partial-success counts; clients must not retry those exports. Request/response sizes, attribute counts, entity counts and timestamps are bounded.

Every request needs the host's `Authorization: Bearer …` and `X-Host-ID` headers. Every resource must have the same **`host.id`**. Container resources additionally need `container.id`; `host.name` and `container.name` are optional display names. A credential cannot submit a different host's data, including in a multi-resource batch. Multi-host gateway credentials are not supported.

Use the [tested Collector configuration](tests/otlp/collector.yaml) as a starting point and consult the [exact supported mapping](tests/otlp/README.md). Adjust its test-only paths, host ID, image filter and pipelines for your deployment. In a normal Collector exporter, configure the hub origin, gzip and the two authentication headers:

```yaml
exporters:
  otlp_http/keiryo:
    endpoint: ${env:KEIRYO_HUB_URL}
    compression: gzip
    headers:
      Authorization: Bearer ${env:KEIRYO_TOKEN}
      X-Host-ID: ${env:HOST_ID}
```

Add `host.id` through a Collector resource processor, using that same `HOST_ID`. Keep the tested metric filter: standard receivers also emit metrics this dashboard does not store. Enable optional `system.memory.limit` and `system.linux.memory.available` for Linux memory charts; free RAM is not substituted for available RAM. Docker `container.cpu.utilization` is already percentage-valued in the tested receiver despite its unit `1`. Choose either that metric or cumulative container CPU time, not both for one container.

Our agent exports raw host CPU/disk/network counters and standard resource attributes. `keiryo.memory.available` is the documented available-memory extension on non-Linux systems. An explicit `otlp_metrics_url` can send metrics to a Collector while private state/log fetching continues against the hub. The configured bearer token is sent to that endpoint too: use a trusted destination and HTTPS or a private encrypted network. OTLP exporter environment variables are not a substitute for this agent's JSON configuration.

Incoming batches merge per measurement rather than replacing the host. Counters use source intervals and persisted baselines; first/reset readings are unavailable, not zero. Delayed counters repair their successor's rate and summary weight. Cumulative host CPU needs the complete supported state set and matching intervals (see the mapping); partial counters never imply CPU zero. Retries are deduplicated by source series and nanosecond timestamp. Missing or stale measurements are not refreshed by another metric or by private-state traffic. Latest metric-derived values expire with raw retention and have persistent per-host bounds. One source group owns each dashboard metric/entity during raw retention: conflicting scopes/resource metadata are rejected rather than silently combined. Source reconfiguration may require allowing raw retention/owner expiry before switching; do not run two producers for one host identity.

Upgraded agents send full private state to `/v1/state`, independently of metric delivery. External collectors can supply numeric container readings without supplying state; such containers remain **unknown**, not implicitly healthy/stopped, and cannot fetch logs. Log controls require a fresh keiryō agent advertising the capability. Private state retains only the newest snapshot in memory; historical snapshots are never replayed after an outage.

### Durable agent metrics

The Rust agent commits each encoded OTLP batch to redb with immediate durability **before** attempting HTTP delivery. It retains original observation timestamps and counter intervals, drains oldest-first and durably deletes only acknowledged batches. Restarting the agent/container resumes pending delivery. A crash after hub acknowledgement but before local deletion can resend a batch; the hub deduplicates identical source-series/timestamp points. This is at-least-once delivery, not exactly-once transport.

The target is recovery through a **one-hour connectivity outage**, with two-hour pending retention and the separate byte ceiling. It depends on successful local commits, adequate disk space, destination acceptance of historical data and sufficient drain throughput. Collection stops while the agent itself is down; local storage cannot survive disk/host loss. The default hub accepts 24-hour-old metrics, but check effective retention and external destinations before rollout.

Expired batches and oldest batches displaced by the byte ceiling are discarded explicitly. An individually oversized batch does not evict existing records. Retryable HTTP failures and malformed success responses retain data; permanent rejection discards that batch, while OTLP partial success reports rejected points and is not retried. Authentication errors can therefore cause loss. Structured stderr events expose queue depth/bytes, expiration/eviction and rejection counts, retries and persistence failures; persistence degradation also appears in private state. Never treat a persistence failure as a successful collection or silently send around the queue.

Queue files are mode 0600; newly created parent directories are restricted. The queue stores metric payloads and host/destination identity, not bearer tokens. A nonempty queue refuses a different host ID or metrics URL: restore the old configuration and drain it first. Token rotation at the same endpoint is supported. Corrupt or unsupported databases are preserved and startup fails rather than deleting history. Clock rollback does not revive expired data; a large forward clock step can expire queued measurements.

The byte ceiling counts protobuf payload only. redb metadata, pages and transaction overhead need additional disk headroom; measure representative hosts and monitor physical file growth. `persistence_failures` counts failures since process startup; discard/rejection totals survive restart in redb. Native tests exercise repeated fixed-size enqueue/drain cycles, not fleet capacity or hardware power-loss guarantees.

During Go-to-Rust migration keep the same host/display identity and do not overlap producers. The compatible `keiryo.agent` scope/version remains stable because the hub includes it in source ownership. On rollback, drain Rust first where possible; the Go agent cannot read redb. Preserve any undrained database for later Rust resumption, subject to retention expiry. No hub database migration is introduced by the Rust agent itself.

### Upgrade and rollback

Back up the hub data directory first (see [storage and backups](#storage-backups-and-limits)). **Upgrade the hub before agents.** The hub accepts legacy `/v1/samples` traffic alongside OTLP; upgraded agents send numeric metrics only over OTLP and do not automatically fall back. No production image is published or deployment performed by running tests.

Opening an existing database migrates it transactionally to **schema 3**, retaining metrics, sessions and settings. Older hub binaries refuse that schema. Downgrade agents first; rolling the hub back requires a compatible pre-upgrade database backup, with the corresponding loss of post-backup history. Do not delete the live database as an upgrade procedure.

Run the opt-in real Collector check from this repository root with `uv run --locked task test SUITE=otlp-integration`. It runs the stock Collector and test hub/agent on loopback inside one isolated-network Docker container, with read-only host and Docker socket access. The socket is privileged in effect. It observes only containers using the test image and does not restart or reconfigure existing services. The image version is pinned once in the application's Taskfile. Normal `test` includes the deterministic wire-fixture replay without Docker.

## What the UI means

- **Connected/stale/offline** measures agent contact, not proof that the host or its applications are healthy. Missing collection is not a zero reading.
- Host CPU is normalized across all cores (0–100%); idle includes I/O wait. Network/disk rates use counter differences and elapsed time, and are absent on the first observation or counter reset.
- Host memory used is total minus available. Docker memory excludes inactive file cache. Docker CPU uses 100% per core, unlike normalized host CPU.
- Filesystem free space is the collector's available/free reading in bytes. Metric names carry units (`_bytes`, `_bytes_s`, `_seconds`, `_percent`).
- Container state, Docker health status when present, exit code for exited containers (or explicitly unknown), restart count, collection errors and agent version appear in the latest snapshot. Raw state/error observations are also retained; five-minute state summaries keep the last observation in each bucket, not every transition. Numeric histories retain raw observations and weighted summaries with min/max/sample counts; missing observations are not filled in.
- Log requests fetch a recent snapshot, not a live tail. Log rotation, unsupported Docker logging drivers, stopped/deleted containers or an offline agent may make logs unavailable. Logs are escaped, bounded, and not written to SQLite or application logs.
- The fleet navigation opens separate, bookmarkable views (`?view=overview`, `resources`, `metrics`, or `conditions`), preserving the time range, host filter and host-page offset. Only the selected view is rendered; `/` defaults to **Fleet Overview**.
- **Fleet Overview** provides threshold totals, a condition map, worst-first host readings and CPU sparklines. **Resource Views** adds a peak-CPU heatmap, host-memory treemap (box size = total RAM; colour = usage), and average CPU comparison. **Fleet Metrics** shows fullest filesystems and top CPU/memory/network consumers; network rankings sum received and transmitted bytes/s across collected interfaces. **Alerts & Hosts** shows current threshold, contact, collection and container conditions, not a persistent alert lifecycle. There are no acknowledgements, alert notifications, forecasts or hypervisor/VM relationships.
- Host CPU, memory and worst-filesystem readings use the configured `alert_policy` thresholds (default warning **≥80%**, critical **≥90%**). Disk health considers all mounts unless `disk_health_mountpoints` selects specific mounts; no valid selected reading means unavailable, never a fallback to excluded mounts. Stale contact or old measurements make the host unavailable; missing metrics do not become zero or healthy. Known warning/critical readings still surface when another metric is missing. Container conditions use observed state/health and one-shot exit codes, not host CPU thresholds (container CPU can exceed 100%). Rankings and memory treemaps exclude stale readings. Historical filesystem readings remain visible with a last-received warning.
- Fleet totals, filters, charts and rankings cover the current page of up to 100 hosts, not unseen pages. CPU history uses weighted summary averages and bucket peaks, aggregated to about 48 time bins per host (at least one configured summary bucket). Missing buckets remain blank. Hover heatmap cells for both peak and average. Open a host for full CPU/memory/disk, network and container charts, raw metrics, state observations and on-demand logs.
- Charts use locally embedded Apache ECharts with no CDN or frontend framework. Drag/scroll to zoom; use series buttons to hide/show lines. Keyboard users can focus line charts and use arrow keys/Home/End to inspect readings. Tables provide equivalent readings, including an expandable fleet CPU table. Axes use browser-local time; history tables retain UTC. Memory/disk percentages use matching timestamps and entities; summary percentages are ratios of bucket averages. Missing or zero denominators produce no reading.
- The browser uses one authenticated **Server-Sent Events** connection per dashboard. Successful sample commits invalidate the relevant host/fleet views; agents still collect every **60 seconds**. Reconnect resynchronizes from SQLite, and a once-per-minute refresh updates contact freshness even without samples. Updates preserve chart zoom/series choices and fetched logs. Use **Paused** or **Refresh now** to control updates. Updates wait while a dashboard control/chart has focus or a raw table is expanded, and resume on release. Unsupported browsers retain manual refresh.
- Raw host history is bounded to 20,000 numeric points and 1,000 state snapshots; charts warn if either limit is reached. Choose a shorter range rather than assuming partial results cover the window. Rendering retains pixel-bucket extrema and breaks lines across missing intervals. Host container/log controls reflect page load; reload the page to update those controls.

### SSE proxy requirements

Allow streaming `GET /events` on the same authenticated UI origin, without response buffering, and allow at least 15 seconds between heartbeat writes. Do not expose the agent listener or weaken proxy authentication for this endpoint. Streams have a separate `max_requests` connection bound, bounded/coalesced notifications, and write deadlines. Local sessions are rechecked on events/heartbeats; proxy-mode streams reconnect through authentication at least every two minutes. Browser disconnects and hub shutdown release subscribers. Validate streaming through your actual reverse proxy before relying on automatic updates; **Refresh now** remains available if streaming fails.

## Authentication and network boundary

**Proxy mode** requires a trusted remote proxy IP and `X-authentik-username`. Supply the exact proxy IP as `/32` (IPv4) or `/128` (IPv6), not an entire shared Docker subnet. The application does not verify an Authentik JWT; it relies on that protected network hop. The proxy must authenticate users, overwrite client identity headers, and be the only trusted source. Never publish the UI port directly. If the proxy IP changes, update configuration; failure is closed. Browser mutations require same-origin protection. The agent listener ignores proxy user identity and requires its own host-bound bearer token.

**Local mode** has a single admin account, salted password hash, rate-limited login, CSRF protection, opaque expiring sessions and restrictive cookies. Bootstrap requires a **loopback UI binding and loopback `public_url`**. Initialize locally (or through an SSH tunnel), then change the binding/HTTPS URL and restart for remote access. This prevents a public reverse proxy from exposing bootstrap through a private socket peer. There is no registration, email recovery, or password-reset endpoint. Keep a secure database backup; offline recovery is an operator procedure, not a public bypass.

**Docker access is privileged in effect.** A read-only bind of the Docker socket does not prevent API mutations by a compromised process. This app only implements required read calls, but root socket access still means host-root risk. The agent also reads host filesystems for host metrics. Do not deploy on machines where that trust is unacceptable.

## Container build

Build from this directory; no parent repository files are needed:

```sh
docker build -t monitoring:dev .
```

The Dockerfile builds the Go hub and target-platform static Rust agent into one scratch image, supporting Linux amd64 and arm64. The final image contains both executables, standard CA bundle and dependency licence notices under `/licenses`. It contains no deployment configuration or credentials. Linux Rust builds require Docker and trusted Cargo registry TLS; do not disable certificate verification to accommodate a private CA. See `THIRD_PARTY.md`; dependency notices do not grant an open-source licence to this project's own source.

## Verification

```sh
uv run --locked task test                         # Rust tests, Go unit/race tests and real native preview
uv run --locked task lint                         # Rust formatting and Clippy
uv run --locked task test SUITE=recovery           # one-hour replay/migration, including lost acknowledgement
uv run --locked task test SUITE=ui                 # chart logic tests; Node.js 18+
uv run --locked task test SUITE=browser            # synthetic fleet + Chromium; pinned Playwright via uv
uv run --locked task build                        # native + Linux amd64/arm64
uv run --locked task build TARGET=native          # native hub + Rust agent; used by preview/browser
uv run --locked task test SUITE=integration        # Linux test binary in local Docker
uv run --locked task bench                        # synthetic SQLite benchmark
```

On Linux distributions that install Go Task as `go-task`, substitute that name for `task`.

Use `task --list` for public interfaces and selector choices. `test` defaults to
`SUITE=unit`; other focused suites are `go`, `agent`, `otlp`, `otlp-integration`,
and `preview`. Docker/browser checks are never part of the default suite.
`build` defaults to `TARGET=all`; `TARGET=linux ARCH=amd64|arm64` builds one
platform (architecture defaults to the Docker server), and `TARGET=agent-native`
builds only the Rust agent. `format COMPONENT=go|agent` defaults to Go;
`lock` updates the Rust lockfile only after intentional manifest changes.
Unknown selectors fail. Run different test suites as separate invocations:
Task variables apply to every command in a single invocation.

The optional browser task requires a Playwright Chromium installation (`uv run --locked --with playwright==1.57.0 python -m playwright install chromium`) or `MONITORING_CHROME` pointing to an existing Chromium/Chrome executable. Browser tests use temporary configuration, credentials and SQLite data, never your deployed hub. Set `MONITORING_SCREENSHOT` to retain a synthetic fleet screenshot.

The integration task cross-compiles locally for the Docker server architecture; it needs the pinned Go test image, host PID visibility, and Docker socket. It drops all capabilities, isolates networking, and runs read-only. It exercises metric delivery and the complete UI/hub/agent log flow without changing host daemon/system configuration. Unit tests use the race detector; the cross-compiled Linux integration binary does not. Native production readings and production network lifecycle still require a real-host trial.

Synthetic benchmarks on Apple M4 Pro, darwin/arm64 (not deployed agent/hub measurements):

| Workload | Upload time | Test-process RSS |
| --- | --- | --- |
| 4 hosts, 8 containers/host | ~0.50 ms | ~35 MiB |
| 16 hosts, 8 containers/host | ~0.50 ms | ~35 MiB |
| 64 hosts, 8 containers/host | ~0.54 ms | ~36 MiB |

The fleet harness includes bearer authentication, JSON validation, 18 metrics/sample, SQLite writes, and periodic UI/history reads, in process without network latency. RSS includes the Go test process and is not the deployed hub's RSS. Single-metric store writes were ~106 µs/upload with ~3.5 KiB allocated/upload. These are not a capacity guarantee; measure actual fleet CPU, RAM, disk growth and retention costs.

## Storage, backups and limits

SQLite uses WAL, serialized writes, indexed bounded history reads, transactional acknowledgements and upload deduplication. Summaries update in the same transaction as raw samples, so retries cannot count twice. Delayed samples older than raw retention are rejected. Cleanup is incremental; large backlogs may need repeated passes. The storage target prunes oldest retained data under pressure and requires spare disk for WAL/index/freelist overhead; it is not a hard filesystem quota. Freed SQLite pages can remain in the database file for reuse.

For a simple safe backup: stop the hub cleanly, copy its **entire data directory** including any WAL/SHM files, then restart it. Do not copy just the main `.db` from a running hub. Alternatively use SQLite's online backup API with appropriate tooling outside this application. Test restoring to an isolated hub before relying on backups. Configuration and credential files need their own secure backup. Restore file ownership/permissions and use a compatible application/schema version.

Durable delivery covers committed batches within the configured age/byte bounds, not disk loss, permanent rejection or time while collection is stopped. The Docker collector caps each pass at 512 containers and reports truncation; larger single-host deployments need that bound exposed/configured and tested. Network-container recreation may require agent recreation; the exact production lifecycle is not validated by isolated-network tests. No HA/cluster, notifications, arbitrary metric plugins, central log archive, container management, configurable dashboards or historical metrics/log import.
