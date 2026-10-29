# OTLP compatibility checks

The integration task owns the pinned Collector contrib image version. The mapping and actual wire output were checked against its released distribution; other releases may change receiver conventions. The real test verifies stock hostmetrics/docker_stats → hub and our agent → stock OTLP receiver → hub, rejecting any partial success for the configured supported set.

`testdata/stock.json` preserves the captured receiver wire shapes (including non-monotonic sums and filesystem attributes), with identities/timestamps normalized and numeric values replaced by small hand-checkable examples. It is not a dump of real machine names or credentials. The replay checks exact CPU scaling, available memory, reserved filesystem space, rates and retry/rebatch deduplication.

Supported metrics (binary protobuf or protobuf JSON over HTTP):

| Metric | Unit | Shape / required attributes | Presentation |
| --- | --- | --- | --- |
| system.cpu.time | s | monotonic cumulative/delta sum; state, optional cpu (absent = aggregate) | CPU percent; idle includes wait |
| system.cpu.utilization | 1 | gauge; state, optional cpu (absent = aggregate) | same CPU definition |
| system.cpu.load_average.1m | {thread} | gauge | load1 |
| system.memory.limit | By | gauge or cumulative non-monotonic sum | total memory |
| system.linux.memory.available | By | gauge or cumulative non-monotonic sum | available memory |
| keiryo.memory.available | By | gauge; agent extension on non-Linux | available memory |
| system.paging.usage | By | gauge or cumulative non-monotonic sum; state=free/used | swap |
| system.uptime | s | gauge | uptime |
| system.filesystem.usage | By | gauge or cumulative non-monotonic sum; mountpoint, state=free/used/reserved | filesystem capacity |
| system.disk.io | By | monotonic cumulative/delta sum; device, direction=read/write | disk B/s |
| system.network.io | By | monotonic cumulative/delta sum; device, direction=receive/transmit | network B/s |
| container.cpu.utilization | 1 | gauge; container resource | Docker CPU percent, **not a fraction** |
| container.cpu.usage.total | ns | monotonic cumulative/delta sum; container resource | Docker CPU percent from elapsed CPU time |
| container.memory.usage.total | By | gauge or cumulative non-monotonic sum; container resource | memory excluding inactive cache |
| container.memory.usage.limit | By | gauge or cumulative non-monotonic sum; container resource | memory limit |

`container.cpu.utilization` is already percentage-valued in the pinned Docker receiver (`receiver.go` calls `calculateCPUPercent`, in `metric_helper_others.go`). Do not multiply it by 100. Enable either that measurement or cumulative container CPU time, not both for the same entity.

Every resource needs `host.id` matching its authorized host. Container resources additionally need `container.id`; `host.name` and `container.name` supply display names. Host-bound bearer credentials and `X-Host-ID` are normal configurable exporter headers. A single credential is not authorized for a multi-host gateway.

Enable optional `system.memory.limit` and `system.linux.memory.available` for Linux host memory charts; free memory is not available memory. Disable/filter metrics not in this table to avoid explicit OTLP partial-success rejections. Histograms and arbitrary application metrics are not dashboard-compatible.

First counter samples, reset intervals and missing data remain unavailable. OTLP-only senders do not provide the private fetch-logs feature or imply healthy container state.

Cumulative host CPU needs the complete supported Linux state set (`idle`, `wait`, `user`, `system`, `nice`, `interrupt`, `softirq`, `steal`) for each CPU, using matching observation/baseline intervals. Missing states or mismatched intervals remain unavailable; normalized `system.cpu.utilization` gauges are a separate supported input. Our agent emits all eight counter states, including zeros on platforms where a state is absent. Delayed counters repair their immediate successor and summary contribution transactionally.

Latest metric-derived values expire with raw retention as well as being freshness-filtered in the UI. Persisted latest snapshots keep at most 10,000 numeric measurements and 2,048 metric-derived container entries per host, retaining the newest entries under churn. Complete private container-state snapshots are independently bounded by ingestion limits and are not pruned based on metric age.

Run from the repository root:

```sh
uv run --locked task test               # includes fixture replay
uv run --locked task test SUITE=otlp-integration # requires Docker; privileged read access
```

The integration task uses a static Go test executable inside the pinned Collector image, matching the existing Linux integration pattern. All HTTP listeners are container-loopback only and Docker networking is disabled. No Python dependency or exposed host listener is needed. The tested YAML is embedded in the test; production must replace its test identities, paths and pipeline topology.
