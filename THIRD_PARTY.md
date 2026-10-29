# Third-party dependencies

Go dependencies and checksums are pinned in `go.mod` / `go.sum`. Direct dependencies:

- `modernc.org/sqlite`: embedded SQLite without CGO (BSD-3-Clause; SQLite itself is public domain).
- `github.com/shirou/gopsutil/v4`: host collection (BSD-3-Clause).
- `golang.org/x/crypto`: password hashing (BSD-3-Clause).
- `go.opentelemetry.io/proto/otlp`: official OTLP wire definitions (Apache-2.0).
- `google.golang.org/protobuf`: protobuf binary/JSON encoding (BSD-3-Clause).
- `google.golang.org/genproto/googleapis/rpc`: OTLP error status messages (Apache-2.0).

Rust dependencies and transitive versions/checksums are locked in `agent-rust/Cargo.lock`. The agent uses redb (MIT/Apache-2.0), official `opentelemetry-proto` definitions (Apache-2.0), prost (Apache-2.0), reqwest/Rustls for HTTP/TLS, serde/serde_json, chrono, uuid, signal-hook, libc and sysinfo for native macOS collection. Dependency notices are copied under `/licenses/rust/` in the release image. The old Go collector remains a compatibility-test oracle, not a production fallback. `tempfile` is test-only. These dependency licences do not license this project's own source.

`internal/hub/static/htmx.min.js` is vendored from `https://unpkg.com/htmx.org@2.0.8/dist/htmx.min.js`, distributed under the Zero-Clause BSD license. No CDN is used at runtime.

`internal/hub/static/echarts.min.js` is the unmodified Apache ECharts **6.0.0** distribution from `https://cdn.jsdelivr.net/npm/echarts@6.0.0/dist/echarts.min.js` (Apache-2.0, with BSD-3-Clause d3 subcomponents). SHA-256: `baa8dfe7e1d9336b98e8986ba7e20ea15e7cdbea1ef42a59d59478632fa45a1d`.

Its `LICENSE`, `NOTICE`, and `licenses/LICENSE-d3` from the same pinned package accompany it as `echarts.LICENSE.txt`, `echarts.NOTICE.txt`, and `echarts.LICENSE-d3.txt`. They are embedded in the binary, available under `/static/`, and copied into `/licenses/echarts/` in the container image. This uses the stock distribution rather than introducing a JavaScript build pipeline; no CDN is used at runtime.

## HTMX Zero-Clause BSD license

Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
