# Hub Triage-First UX Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the hub's Overview answer "is anything wrong, and what?" first, make the host page show when a problem started, and polish the remaining views.

**Architecture:** Keep the Go `html/template` + vanilla JS + vendored ECharts stack. Go view-models gain triage fields (primary reason, attention list, stat state, problem chart). Templates and `style.css` are restructured around shared component classes. `dashboard.js` gains local-time rendering, byte-rate units and a pure `problemSince` helper.

**Tech Stack:** Go 1.x stdlib (`html/template`, `net/http`), vanilla JS, ECharts (vendored), node:test, Playwright smoke test.

**Spec:** `docs/superpowers/specs/2026-11-16-hub-triage-ux-design.md`

All paths are relative to `apps/monitoring/`. Run commands from there.

## Global Constraints

- No new runtime/build dependency, no CSS framework, no frontend build step.
- No storage/schema/notification-policy changes. Health/threshold semantics unchanged.
- Fleet view id `conditions` stays (bookmarks); only its title changes to `Containers & backups`.
- Every colour-coded state also carries text (`✓ Normal`, `! Warning`, `! Critical`, `? No data`).
- Percentages render with one decimal everywhere.
- Summary line, host cards and Needs attention ignore the `q` filter; filter narrows only the readings table and other tabs.
- A healthy-looking Overview must never hide: `.Degraded`, stale/never-seen hosts, refresh failures.
- Keep existing a11y: chart keyboard readout, legend `aria-pressed`, `#refresh-status` `role=status`, `<noscript>` fallbacks.
- Anything that must live-refresh stays inside `#dashboard-data`.
- Ship as one change; commit once at the end, only after the user confirms.

## Review Focus

- Filter set to a healthy host while another host is critical → summary/cards/attention still show the critical host (Task 1 test).
- Stale host whose last reading was 95% CPU → card shows `? No data`, chart not reordered, no "since" (Task 2 test).
- Hub degraded with zero conditions → no "All N hosts … within thresholds" sentence; degraded alert shown (Task 3 test).
- Zero/never times (`never seen`, no sample) → `<time>` not emitted for zero time; text says "Never" (Task 3 test).
- Live refresh swaps `#dashboard-data` → local/relative times and since-labels re-applied after swap (Task 5 browser assertion).

---

### Task 1: Fleet triage view-model

**Files:**
- Modify: `internal/hub/fleet.go` (`fleetHost`, `fleetRow`, `fleet`)
- Modify: `internal/hub/hub.go:71` (FuncMap)
- Test: `internal/hub/fleet_test.go`

**Interfaces:**
- Produces: `fleetHost.Reason string` (primary reason text, empty when healthy). Templates use existing `Health` for state.
- Produces: `func rate(v float64) string` → `"512 B/s"`, `"3.0 KiB/s"`, `"1.5 MiB/s"`, `"2.0 GiB/s"` (1024 base, one decimal above B/s, integer B/s).
- Produces: template data keys on `fleet`: `Cards []fleetHost` (all page hosts, unfiltered, worst first), `Hosts []fleetHost` (filtered, worst first — existing key), `Attention []condition` (from `Cards`), `Counts` (from `Cards`), `Total int` (=len(Cards)).
- Produces: FuncMap `rate`, `iso` (`func(time.Time) string` → RFC3339 UTC, `""` for zero).
- Resource condition `Detail` becomes `"95.0% used · critical at 90%"` / `"85.0% used · warning at 80%"` (healthy keeps `"32.0% used"`). This text also flows to Discord messages — intended.

- [ ] **Step 1: Write failing tests** in `fleet_test.go`:

```go
func TestOverviewTriageIgnoresFilter(t *testing.T) // ingest "gateway" cpu 95 and "erp" cpu 20; GET /?q=erp
// assert body contains card for gateway: `href="/hosts/gateway"` inside the host-cards region
//   (match regexp `class="host-card[^"]*critical"[^>]*href="/hosts/gateway"` or equivalent attribute order you choose in Task 3)
// assert readings table excludes gateway: regexp `<tr[^>]*data-host="gateway"` absent, `data-host="erp"` present
// assert "Needs attention" present and contains "CPU at 95.0%" reason or "95.0% used · critical at 90%"

func TestPrimaryReason(t *testing.T) // table-driven on h.fleetRow:
// cpu 95 → Reason "CPU at 95.0%"; cpu 85 + container exited(critical) → container reason wins ("database: exited…" via containerDetail);
// stale host (Sample.At -1h) → Reason "No recent readings"; never seen store.Host{ID:"x"} → "No readings received yet"; healthy → ""

func TestRateUnits(t *testing.T) // rate(512)=="512 B/s", rate(3072)=="3.0 KiB/s", rate(1.5*(1<<20))=="1.5 MiB/s"

func TestResourceDetailIncludesThreshold(t *testing.T) // cpu 95 condition Detail == "95.0% used · critical at 90%"
```

Ordering rule for `Reason` (spec): highest severity among `Conditions`; ties by kind order resource(CPU, Memory, Disk) → contact → container → collection → backup. Resource reason text: `"<Name> at <v>%"`; disk adds mount when known is NOT required here (host page handles mount). Other kinds: `"<Kind>: <Detail>"` except contact (use the two fixed strings above).

- [ ] **Step 2: Run** `go test ./internal/hub/ -run 'TestOverviewTriage|TestPrimaryReason|TestRateUnits|TestResourceDetail' -v` — Expected: FAIL (undefined `rate`, missing `Reason`).

- [ ] **Step 3: Implement** `Reason` in `fleetRow`, threshold text in the resource `add(...)` (thresholds from `h.config.AlertPolicy.Resources.<X>.Warning/Critical`), `rate`, `iso`, and in `fleet()` build `Cards` from every page host before the `q` check; `Counts`/`Attention` from `Cards`; `Hosts`/`storage`/charts/ranks keep using filtered rows. Sort `Cards` with the same stable severity sort as rows.

- [ ] **Step 4: Run** `go test ./internal/hub/ -run 'TestOverviewTriage|TestPrimaryReason|TestRateUnits|TestResourceDetail' -v` — Expected: tests not depending on Task 3 markup PASS; `TestOverviewTriageIgnoresFilter` stays red until Task 3 (note in task report).

---

### Task 2: Host page view-model

**Files:**
- Modify: `internal/hub/charts.go` (`chartView`, `statView`, `overview`)
- Modify: `internal/hub/ui.go` (`host` handler)
- Test: `internal/hub/charts_test.go`

**Interfaces:**
- Consumes: `h.fleetRow`, `h.volumes`, `h.metricReading` (existing).
- Produces: `type statView struct{ Label, Value, State string }` (State ∈ `""|healthy|warning|critical|unavailable`; `""` for Containers).
- Produces: `func (h *Hub) hostStats(host store.Host) []statView` replacing free `overview()`; labels `CPU`, `Memory`, `Disk <mount>` (fullest monitored mount via `h.volumes`; label `Disk` when none), `Containers`. Value text: `"95.0%"`; Containers `"1 running"` or `"1 running · 2 other"`, keep `" · collection incomplete"` suffix rule, `"Unavailable"` rule unchanged.
- Produces: `chartView.State string` and `chartView.Entity string`; host handler sets them for cpu/memory/disk charts when the host row is fresh and the resource state is warning/critical (Entity = offending mount for disk, `"CPU"`/`"Memory"` series names otherwise), then stable-moves charts with non-empty State to the front (critical before warning).

- [ ] **Step 1: Update/write tests** in `charts_test.go`:
  - Replace `overview(host)` assertions with `h.hostStats(host)` expecting `{"CPU","25.0%","healthy"}`, `{"Memory","30.0%","healthy"}`, `{"Disk /","75.0%","healthy"}`, `{"Containers","1 running",""}`; empty host → CPU `"Unavailable"`.
  - `TestHostStatsWorstMount`: `/` 20% + `/data` 95% → `{"Disk /data","95.0%","critical"}`.
  - `TestProblemChartFirst`: render `/hosts/node` with cpu 95 fresh → first `class="chart-panel` element has `data-resource="cpu"` and `data-state="critical"`; same host with `Sample.At` −1h → no `data-state=` attribute in body (stale not reordered).

- [ ] **Step 2: Run** `go test ./internal/hub/ -run 'TestDashboardMetrics|TestHostStats|TestProblemChart' -v` — Expected: FAIL.

- [ ] **Step 3: Implement** per Interfaces. `overview()` callers: only `ui.go:83`.

- [ ] **Step 4: Run** same command — `TestDashboardMetrics`, `TestHostStatsWorstMount` PASS; `TestProblemChartFirst` needs Task 3's `data-state` attribute (note it).

---

### Task 3: Templates and CSS

**Files:**
- Modify: `internal/hub/templates/pages.html`
- Modify: `internal/hub/static/style.css`
- Modify: `internal/hub/fleet.go:215-218` (view titles)
- Test: `internal/hub/fleet_test.go`, `internal/hub/policy_test.go`, `internal/hub/backups_test.go`

**Interfaces:**
- Consumes: Task 1 keys (`Cards`, `Attention`, `Total`, `Reason`, `rate`, `iso`), Task 2 (`statView.State`, `chartView.State/Entity`).
- Produces (DOM contract for Task 4/5): `<time datetime="{{iso .}}" data-format="local|relative">UTC text</time>` (omit element for zero time, render `Never`); host cards `a.host-card.<health>`; attention rows `.attention-row`; readings rows `<tr data-host="{{.ID}}">`; chart panels `data-state`, `data-entity`; one `.chart-hint` above the host chart grid; `#containers` anchor on host page.

- [ ] **Step 1: Update tests first:**
  - `fleet_test.go`: view titles list → `"Fleet Overview", "Resource Views", "Fleet Metrics", "Containers &amp; backups"` (both tests, incl. nav regexp). Overview content marker stays `"critical"`.
  - `TestNeedsAttentionHiddenWhenHealthy`: one fresh healthy host → body lacks `Needs attention`, contains `within thresholds`; set `h.degraded = "Alert delivery failed…"` → body lacks `within thresholds`, contains the degraded text with `role="alert"`.
  - `TestZeroTimesRenderNever`: never-seen host on `/` → readings row contains `Never` and no `datetime=""`.
  - `backups_test.go` / `policy_test.go`: keep (`container-chip healthy`, backup text on `?view=conditions`); `<meter class="healthy" … value="90">` must still match on `/`.

- [ ] **Step 2: Run** `go test ./internal/hub/ -v` — Expected: new/updated tests FAIL.

- [ ] **Step 3: Implement templates** per spec §Overview, §Resource/Metrics, §Containers & backups, §Host page:
  - Overview order: heading + summary (`All {{.Total}} hosts are reporting within thresholds` only when no attention, not degraded; else `X need attention · Y with no data`; add `· first 100 hosts` when `.More`) → cards → Needs attention (only `{{if .Attention}}`) → toolbar (filter/range) + readings table (existing meter/sparkline/uptime cells; "Last reading" column; contact pill only when `.Status` ≠ `connected`; over-threshold suffix `· Critical`) → `<details><summary>How status works</summary>` holding the existing caveat sentences verbatim.
  - Remove: `.fleet-stats` strip, `.fleet-map`, duplicated eyebrow+section-title pairs, conditions table on `conditions` view, history-range nav inside host raw `<details>`, per-chart "Drag to zoom" readout text (readout element stays, empty).
  - Host page: stat cards use `.State` class + suffix; "Containers & logs ↓" link to `#containers`; chart panels emit `data-state`/`data-entity`.
  - Network ranking uses `rate`; filesystem `%` uses `reading` (one decimal).
- [ ] **Step 4: Rewrite `style.css`**: `:root` tokens (existing palette values; spacing 4/8/12/16/24/32; radius), components `.card`, `.pill` (`white-space:nowrap`, glyph prefix), `.meter`, `.stat`, `.host-card` (top border = state), `.attention-row`; ≤600px: toolbar two rows, readings table first two columns `position:sticky`.
- [ ] **Step 5: Run** `go test ./internal/hub/ -v` — Expected: PASS (incl. Task 1/2 deferred tests).

---

### Task 4: dashboard.js behaviour

**Files:**
- Modify: `internal/hub/static/dashboard.js`
- Test: `tests/ui/dashboard.test.cjs`

**Interfaces:**
- Consumes: Task 3 DOM contract, `data-thresholds` JSON.
- Produces (exported via `module.exports`): `formatValue(value, unit)` (unit `"B/s"` scales like Go `rate`); `problemSince(samples, threshold, gap, start) → {kind:"since", at}|{kind:"before"}|{kind:"latest"}`; `relativeTime(ms, now) → "18s ago" | "5m ago" | "3h ago" | "2d ago"`.

- [ ] **Step 1: Write failing tests** in `dashboard.test.cjs`:

```js
test("problemSince walks back to first over-threshold sample", ...)
// samples [{At:0,Value:50},{At:60,Value:92},{At:120,Value:95}], threshold 90, gap 90, start 0 → {kind:"since",at:60}
// all over → {kind:"before"}; last value 70 → {kind:"latest"}; gap before last ({At:0,95},{At:500,95}, gap 90) → {kind:"since",at:500}
test("byte rates scale units", ...) // formatValue(3072,"B/s")==="3.0 KiB/s"; formatValue(512,"B/s")==="512 B/s"; formatValue(25,"%") unchanged from today
test("relative time buckets", ...) // relativeTime(now-18000, now)==="18s ago"; 300000→"5m ago"; 3h; 2d
test("threshold labels have room", ...) // chartOption percent line: option.grid.right >= 45
```

- [ ] **Step 2: Run** `node --test tests/ui/dashboard.test.cjs` — Expected: FAIL.
- [ ] **Step 3: Implement**: the three helpers; `applyTimes(root)` sets `<time>` text to local (`data-format="local"`) or relative (`"relative"`) and `title` to the original UTC text; on panels with `data-state`, compute since (series named by `data-entity`, threshold from `data-thresholds[resource][state]`) and insert a `.since` line: `Critical since HH:MM` / `Started before this range` + link to next range (`1h→24h→7d→30d`) / `Critical at last reading HH:MM`; heatmap `grid.right` so the last x label is not clipped; treemap label colour readable. Call `applyTimes` + since inside `render(root)` so live refresh re-applies.
- [ ] **Step 4: Run** `node --test tests/ui/dashboard.test.cjs` — Expected: PASS.

---

### Task 5: Browser smoke, validation, visual check, commit

**Files:**
- Modify: `tests/ui/browser.py`

- [ ] **Step 1: Update browser test**: replace `.fleet-cell` assertions with `.host-card` (4 cards; 1 `.host-card.unavailable`; after critical upload `.host-card.critical`); nav title `"Containers & backups"`; mobile check → `.host-card` grid has 2 columns at 390px; after an SSE refresh assert a `time[data-format="relative"]` text ends with `ago`; on `/hosts/gateway?range=24h` after the 95% upload assert first `.chart-panel` has `data-resource="cpu"` and a `.since` element exists. Keep remaining assertions (zoom/legend persistence, keyboard readout "95", logs preserved, 7 canvases).
- [ ] **Step 2: Run** `uv run --locked task test SUITE=ui && uv run --locked task test SUITE=go && uv run --locked task test SUITE=browser` — Expected: all PASS, browser prints "Browser smoke passed…".
- [ ] **Step 3: Visual check**: rerun the screenshot script used in design review (`/tmp/keiryo-shots/shoot.py`) and view overview (problem + all-clear), host, resources, metrics, containers, overview at 390px. Expected: no clipped labels, no broken pills, empty attention section absent when healthy.
- [ ] **Step 4: Repo validation** from repo root: `uv run --locked task validate` — Expected: PASS.
- [ ] **Step 5: Ask user, then commit** `apps/monitoring` changes + spec + plan as one commit: `feat(monitoring): triage-first hub overview and UI polish`.
