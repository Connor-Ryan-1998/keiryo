# Hub triage-first UX — design

Date: 2026-11-16 · Scope: `apps/monitoring` hub UI · Ships as one change.

## Intent

The operator opens the hub to answer one question: *is anything wrong, and if so what?*
If something is wrong they act on it; otherwise they want the state of every host at a
glance at the top of the page. Today the answer is spread across a count strip, a fleet
map, a table and a separate "Alerts & Hosts" tab, and buried under repeated caveat text.

## Goals

- Overview leads with the state of every host, then the reasons for any problem.
- An empty "needs attention" section disappears; a healthy fleet reads as healthy.
- From a problem, one click reaches the host page, whose first chart shows when it started.
- The other three views stay and get polished (consistent time, units, wrapping, caveats).

## Non-goals

- No new remediation controls (restart, silence, acknowledge). Inspect links only navigate.
- No new storage, schema, alerting or notification behaviour.
- No change to health/threshold policy semantics.

## Anti-goals

- A missing "needs attention" section must never mean "data unavailable". Degraded hub
  state, refresh failures, and stale/never-seen hosts are always shown as problems.
- No frontend build step, CSS framework or new runtime dependency (daisyUI/Tailwind
  considered and rejected: needs a build toolchain, saves ~30 lines of CSS, does not help
  the Go/template work or the ECharts canvas styling).
- Do not drop any accessibility behaviour already present (keyboard chart readout,
  legend buttons with `aria-pressed`, `role=status` refresh line, noscript fallbacks).

## Constraints

- Stack stays: Go `html/template` (`internal/hub/templates/pages.html`), one `style.css`,
  `dashboard.js` (vanilla + vendored ECharts/htmx). Single binary, self-hosted assets.
- Live refresh replaces `#dashboard-data` from a refetched page; anything that must
  update live lives inside that element; chart panels are matched by title.
- Existing tests to keep green and update: `internal/hub/*_test.go` (notably
  `fleet_test.go` asserts view titles/links/content), `tests/ui/dashboard.test.cjs`,
  `tests/ui/browser.py` (asserts `.fleet-cell` counts, nav titles, toolbar labels).
- State must not be conveyed by colour alone (text label alongside every colour).

## Approaches considered

- **A. Rework existing templates/CSS/JS + small Go view-model additions** — chosen.
  Fits the lightweight single-binary design; tests updated rather than replaced.
- B. Add Tailwind/daisyUI — rejected (build step, dependency, little saving).
- C. CSS-only polish — rejected: cannot add per-host reasons, hide the empty section,
  or reorder host charts, so does not deliver triage-first.

Ship as one change (user decision).

## Design

### Shared

- **Clock:** show local browser time everywhere; exact UTC in a `title` tooltip.
  Server renders `<time datetime="RFC3339">UTC text</time>`; `dashboard.js` rewrites
  text to local and relative forms ("18s ago" where specified). No-JS shows UTC.
- **Units/format:** percentages one decimal everywhere (filesystems currently two).
  Byte rates scale B/s → KiB/s → MiB/s → GiB/s (Go helper + JS `formatValue`).
- **Caveats:** the per-page muted disclaimer paragraphs collapse into one
  `<details><summary>How status works</summary>` per page, keeping their wording.
- **Status vocabulary:** health pill (Normal / Warning / Critical / No data) is primary.
  Contact pill (stale / offline / never seen) shows only when the host is *not*
  connected. "connected" is no longer rendered as a pill. Pills use `white-space:nowrap`
  and a leading glyph/text (`✓`, `!`, `?`) so state is not colour-only.
- **CSS:** reorganise `style.css` into variables (colours, spacing, radius) plus a few
  component classes (`.pill`, `.card`, `.meter`, `.stat`); keep the existing dark palette.
- **Mobile (≤600px):** toolbar compacts (range + filter on one row, update controls on a
  second); readings table keeps host + CPU visible with the rest horizontally scrollable.

### Overview (`?view=overview`, default)

**Scope (user decision):** the summary line, host cards and Needs attention are
fleet-wide for the loaded page and **ignore the `q` filter**, so a filter can never hide a
problem. The filter narrows only the readings table (and the other tabs, as today).
Pagination is unchanged; when more hosts exist beyond this page (`More`), the summary
says it covers the first 100 hosts.

Top to bottom, all inside `#dashboard-data` so it live-refreshes:

1. **Heading + summary line**: "All N hosts are reporting within thresholds" or
   "X hosts need attention · Y with no data". Update status (`#refresh-status`) and
   Refresh control on the right. Degraded hub message (`.Degraded`) shown as an alert here.
2. **Host cards** (replace count strip *and* fleet map): one card per host, worst first.
   Name, health pill, one line of content, "Received Xs ago". Content line is the
   primary reason when unhealthy (e.g. "CPU at 95%", "No readings received yet",
   "Container db exited") else "CPU 32% · Memory 75%". Top border colour = state.
   Card links to `/hosts/{id}`.
3. **Needs attention** — rendered only when there is ≥1 condition: one row per
   condition: health pill, "host · signal" title, detail incl. threshold where it is a
   resource ("95.0% used · critical at 90%"), and "Inspect host →" link.
4. **Latest host readings** table — keep current columns and visuals the user likes:
   CPU / Memory / Worst disk meter bars + %, CPU history sparkline, Uptime in days.
   "Contact / collected" becomes "Last reading" with relative time and UTC tooltip,
   plus the contact pill only when not connected. Over-threshold values add a text
   suffix ("95.0% · Critical").
5. Collapsed "How status works" note. Pagination nav unchanged.

### Resource Views / Fleet Metrics — polish only

- Resource: heatmap x-axis labels not clipped at the right edge; threshold markLine
  labels not clipped; treemap label legible (contrast + truncate). Caveat → details.
- Metrics: filesystem % one decimal; network ranking in scaled byte units.
- Section headings: drop the duplicated eyebrow + title pair below the page title.

### Alerts & Hosts → "Containers & backups"

Rename tab (id stays `conditions` to keep URLs). Remove the "Conditions needing
attention" table (now on Overview); keep per-host cards with containers and backup
signals, using the shared pill rules. Link back to Overview's attention list.

### Host page (`/hosts/{id}`)

- Stat cards gain state: value coloured and suffixed with the state when over threshold
  (CPU/Memory/Disk). The disk card shows the **fullest monitored mount** (same rule as
  host health: worst within `DiskHealthMountpoints`) and names it, e.g.
  "Disk /data · 92.0% · Critical", replacing the root-only card. Containers card
  shortens to "1 running" / "1 running · 2 other" so it fits.
- **Problem-first charts:** if a resource (cpu/memory/disk) is warning/critical in the
  host's health (fresh hosts only — stale/never-seen hosts are not reordered), its chart
  is moved first and marked with a pill + "Critical since HH:MM" / "Warning since …".
  "Since" is computed client-side from the loaded series (for disk: the offending
  mount's series): walk back from the latest sample while value ≥ that state's
  threshold. If every sample in range is over, show "Started before this range" with a
  link to the next longer range. If the latest chart point is below threshold or the
  series ends in a gap (e.g. bucket averages vs instantaneous reading), show no "since"
  and instead "Critical at last reading HH:MM" — never guess a start time.
- Chart hint ("Drag to zoom…") shown once above the grid, not per chart.
- Threshold markLine labels not clipped (grid right padding).
- "Containers & logs ↓" jump link next to the host status at the top.
- Duplicate history-range nav inside the raw-data `<details>` removed (toolbar covers it).

## Decisions left to the builder

- Exact wording of summary/reason strings, within the meanings above.
- Which single condition becomes a card's "primary reason" (rule: highest severity,
  ties by order resource → contact → container → collection → backup).
- Breakpoints, spacing values and component class names.
- Whether relative times tick client-side between refreshes (nice-to-have).

## Calls made by Claude (flag if wrong)

- Count strip and fleet map are replaced by host cards (the cards carry the same info).
- Containers tab drops the conditions table rather than filtering it.
- "Since" times come from chart data, not notification state (which only exists when
  Discord is configured).
- Tab id `conditions` kept so bookmarks keep working.
