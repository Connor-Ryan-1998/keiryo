"use strict";

// Retain endpoints and extrema per horizontal pixel, splitting gaps first.
function chartSegments(samples, start, end, gap) {
  const segments = [];
  let segment = [];
  for (const point of samples) {
    if (segment.length && point.At - segment[segment.length - 1].At > gap) {
      segments.push(segment);
      segment = [];
    }
    segment.push(point);
  }
  if (segment.length) segments.push(segment);
  return segments.map(points => {
    const buckets = new Map();
    for (let i = 0; i < points.length; i++) {
      const pixel = Math.floor(600 * (points[i].At - start) / (end - start));
      const b = buckets.get(pixel);
      if (!b) buckets.set(pixel, {first: i, last: i, min: i, max: i});
      else {
        b.last = i;
        if (points[i].Value < points[b.min].Value) b.min = i;
        if (points[i].Value > points[b.max].Value) b.max = i;
      }
    }
    const indices = new Set();
    for (const b of buckets.values()) for (const i of [b.first, b.min, b.max, b.last]) indices.add(i);
    return [...indices].sort((a, b) => a - b).map(i => points[i]);
  });
}

function nearestSample(samples, at, gap) {
  let lo = 0, hi = samples.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (samples[mid].At < at) lo = mid + 1; else hi = mid;
  }
  const candidates = [samples[lo], samples[lo - 1]].filter(Boolean);
  candidates.sort((a, b) => Math.abs(a.At - at) - Math.abs(b.At - at));
  const best = candidates[0];
  return best && Math.abs(best.At - at) <= gap / 2 ? best : null;
}

const chartColors = ["#5794f2", "#73bf69", "#fade2a", "#ff9830", "#b877d9", "#f2495c"];
const usageColor = (value, thresholds) => value >= thresholds.critical ? "#f2495c" : value >= thresholds.warning ? "#ff9830" : "#73bf69";
const formatValue = (value, unit) => `${new Intl.NumberFormat(undefined, {maximumFractionDigits: 2, notation: Math.abs(value) >= 10000 ? "compact" : "standard"}).format(value)} ${unit}`;

function chartOption({kind = "line", series, unit = "", percent = false, start, end, gap, step, thresholds = {warning:80,critical:90}}) {
  const base = {
    animation: false, color: chartColors, backgroundColor: "transparent",
    textStyle: {color: "#b8b8ba", fontFamily: "system-ui, sans-serif"},
    aria: {enabled: true},
    // Rich-text tooltips paint to canvas, never interpolate metric names as HTML.
    tooltip: {trigger: "axis", renderMode: "richText", backgroundColor: "#111217", borderColor: "#2c3235", textStyle: {color: "#ccccdc"}},
    grid: {left: 65, right: 25, top: 35, bottom: 65, containLabel: true},
    xAxis: {type: "time", min: start, max: end, axisLabel: {color: "#b8b8ba", hideOverlap: true}, axisLine: {lineStyle: {color: "#2c3235"}}},
    yAxis: {type: "value", min: 0, ...(percent ? {max: 100} : {}), name: unit, axisLabel: {color: "#b8b8ba"}, splitLine: {lineStyle: {color: "#2c3235"}}},
    legend: {show: false},
    dataZoom: [{type: "inside", filterMode: "none"}, {type: "slider", bottom: 5, height: 18, borderColor: "#2c3235", backgroundColor: "#111217", fillerColor: "rgba(87,148,242,0.15)", handleStyle: {color: "#b8b8ba", borderColor: "#2c3235"}, moveHandleStyle: {color: "#b8b8ba"}, dataBackground: {lineStyle: {color: "#8e8e90"}, areaStyle: {color: "#2c3235"}}, selectedDataBackground: {lineStyle: {color: "#5794f2"}, areaStyle: {color: "#5794f2"}}, textStyle: {color: "#b8b8ba"}}],
  };
  if (kind === "treemap") {
    return {...base, xAxis: [], yAxis: [], dataZoom: [], tooltip: {...base.tooltip, trigger: "item"}, series: [{
      type: "treemap", roam: false, nodeClick: false, breadcrumb: {show: false},
      label: {formatter: p => `${p.name}\n${formatValue(p.value / (1 << 30), "GiB")} · ${p.data.used.toFixed(1)}%`, color: "#111217", overflow: "truncate"},
      itemStyle: {borderColor: "#181b1f", borderWidth: 3, gapWidth: 3},
      data: series.map(p => ({name: p.Name, value: p.Value, used: p.Used, itemStyle: {color: usageColor(p.Used, thresholds)}})),
    }]};
  }
  if (kind === "heatmap") {
    const hosts = [...new Set(series.map(p => p.Host))];
    const times = [];
    for (let at = Math.floor(start / step) * step; at < end; at += step) times.push(at);
    return {...base, dataZoom: [], grid: {...base.grid, left: 20, bottom: 70},
      tooltip: {...base.tooltip, trigger: "item", formatter: p => `${hosts[p.value[1]]}\n${new Date(times[p.value[0]]).toLocaleString()}\nPeak ${p.value[2].toFixed(1)}% · average ${p.value[3].toFixed(1)}%`},
      xAxis: {type: "category", data: times.map(t => new Date(t).toLocaleString(undefined, {month: "short", day: "numeric", hour: "2-digit", minute: "2-digit"})), axisLabel: {hideOverlap: true, color: "#b8b8ba"}},
      yAxis: {type: "category", data: hosts, axisLabel: {width: 140, overflow: "truncate", color: "#b8b8ba"}},
      visualMap: {type: "piecewise", dimension: 2, orient: "horizontal", left: "center", bottom: 0, pieces: [
        {gte:0,lt:thresholds.warning,color:"#73bf69"},
        {gte:thresholds.warning,lt:thresholds.critical,color:"#ff9830"},
        {gte:thresholds.critical,color:"#f2495c"},
      ], textStyle: {color: "#b8b8ba"}},
      series: [{type: "heatmap", data: series.map(p => [times.indexOf(p.At), hosts.indexOf(p.Host), p.Peak, p.Value]).filter(p => p[0] >= 0), itemStyle: {borderColor: "#181b1f", borderWidth: 2}, emphasis: {itemStyle: {borderColor: "#ccccdc"}}}],
    };
  }
  const spark = kind === "spark";
  const lines = series.map((s, index) => {
    const data = [];
    const segments = chartSegments(s.Samples || [], start, end, gap);
    const singletons = new Set(segments.filter(segment => segment.length === 1).map(segment => segment[0].At));
    for (const segment of segments) {
      if (data.length) data.push([segment[0].At - 1, null]);
      data.push(...segment.map(p => [p.At, p.Value]));
    }
    return {name: s.Name, type: "line", data, connectNulls: false, showSymbol: true, symbolSize: spark ? value => singletons.has(value[0]) ? 4 : 0 : 3, lineStyle: {width: 2},
      ...(percent && !spark && index === 0 ? {markLine: {silent: true, symbol: "none", label: {formatter: p => `${p.value}%`}, data: [{yAxis: thresholds.warning, lineStyle: {color: "#ff9830", type: "dashed"}}, {yAxis: thresholds.critical, lineStyle: {color: "#f2495c", type: "dashed"}}]}} : {}),
    };
  });
  return {...base, ...(spark ? {grid: {left: 2, right: 2, top: 5, bottom: 5}, xAxis: {...base.xAxis, show: false}, yAxis: {...base.yAxis, show: false}, tooltip: {show: false}, dataZoom: []} : {}), series: lines};
}

function panelKey(panel) { return panel.dataset.title || panel.querySelector("h2").textContent; }

function renderChart(panel, root) {
  const series = JSON.parse(panel.dataset.series);
  const kind = panel.dataset.kind || "line";
  const plot = panel.querySelector(".plot");
  if (typeof echarts === "undefined") {plot.textContent = "Chart library unavailable; use the readings and history tables."; return;}
  const start = Number(root.dataset.start), end = Number(root.dataset.end), gap = Number(root.dataset.gap);
  const hasData = kind === "line" || kind === "spark" ? series.some(s => s.Samples?.length) : series.length > 0;
  if (!hasData) {
    echarts.getInstanceByDom(plot)?.dispose();
    plot.textContent = "No observations in this range.";
    plot.classList.add("empty-chart");
    panel.querySelector(".legend")?.remove();
    return;
  }
  plot.classList.remove("empty-chart");
  let chart = echarts.getInstanceByDom(plot);
  const previous = chart?.getOption();
  if (!chart) {plot.replaceChildren(); chart = echarts.init(plot, null, {renderer: "canvas"});}
  const thresholds = JSON.parse(root.dataset.thresholds)[panel.dataset.resource];
  const option = chartOption({kind, series, unit: panel.dataset.unit, percent: panel.dataset.percent === "true", start, end, gap, step: Number(root.dataset.step), thresholds});
  if (previous?.legend?.[0]?.selected) option.legend.selected = previous.legend[0].selected;
  option.dataZoom.forEach((zoom, index) => {const old = previous?.dataZoom?.[index]; if (old) {zoom.start = old.start; zoom.end = old.end;}});
  chart.setOption(option, {notMerge: true});
  plot.setAttribute("role", "img");
  plot.setAttribute("aria-label", `${panelKey(panel)}. ${kind === "line" ? "Arrow keys inspect values; drag to zoom. Series buttons follow." : "Equivalent readings available in the tables."}`);
  if (kind === "spark") {plot.setAttribute("aria-hidden", "true");return;}
  if (kind === "heatmap") plot.style.height = `${Math.max(300, option.yAxis.data.length * 28 + 130)}px`;
  chart.resize();
  if (kind !== "line") return;
  plot.tabIndex = 0;
  let legend = panel.querySelector(".legend");
  if (!legend) {legend = document.createElement("div");legend.className = "legend";plot.after(legend);}
  legend.replaceChildren();
  series.forEach(s => {
    const button = document.createElement("button");button.type = "button";button.textContent = s.Name;
    button.setAttribute("aria-pressed", String(option.legend.selected?.[s.Name] !== false));
    button.addEventListener("click", () => {chart.dispatchAction({type: "legendToggleSelect", name: s.Name});button.setAttribute("aria-pressed", String(chart.getOption().legend[0].selected[s.Name] !== false));});
    legend.append(button);
  });
  const timestamps = [...new Set(series.flatMap(s => (s.Samples || []).map(p => p.At)))].sort((a,b) => a-b);
  let selected = timestamps.length - 1;
  const readout = panel.querySelector(".chart-readout");
  plot.onfocus = () => {if (readout) readout.setAttribute("aria-live", "polite");};
  plot.onblur = () => {if (readout) readout.setAttribute("aria-live", "off");};
  plot.onkeydown = event => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key) || !timestamps.length) return;
    event.preventDefault();
    selected = event.key === "Home" ? 0 : event.key === "End" ? timestamps.length-1 : Math.max(0, Math.min(timestamps.length-1, selected + (event.key === "ArrowLeft" ? -1 : 1)));
    const at = timestamps[selected];
    if (readout) readout.textContent = `${new Date(at).toLocaleString()} · ` + series.filter(s => chart.getOption().legend[0].selected[s.Name] !== false).map(s => {
      const p = nearestSample(s.Samples || [], at, gap);return `${s.Name}: ${p ? formatValue(p.Value, panel.dataset.unit) : "unavailable"}`;
    }).join(" · ");
  };
}

function renderHistory(root) {
  const target = root.querySelector("#fleet-history-table");
  if (!target) return;
  const heat = root.querySelector('[data-kind="heatmap"]');
  const table = document.createElement("table");
  const header = table.createTHead().insertRow();
  for (const label of ["Host", "UTC time", "Average CPU %", "Peak CPU %"]) {const th = document.createElement("th");th.textContent = label;header.append(th);}
  const body = table.createTBody();
  for (const p of JSON.parse(heat.dataset.series)) {
    const row = body.insertRow();for (const value of [p.Host, new Date(p.At).toISOString(), p.Value.toFixed(1), p.Peak.toFixed(1)]) row.insertCell().textContent = value;
  }
  target.replaceChildren(table);
}

function initDashboard() {
  let section = document.getElementById("dashboard-data");
  if (!section) return;
  const render = root => {root.querySelectorAll(".chart-panel").forEach(panel => renderChart(panel, root));renderHistory(root);};
  render(section);
  document.getElementById("node-select")?.addEventListener("change", event => {location.href = `/hosts/${encodeURIComponent(event.target.value)}${location.search}`;});
  const status = document.getElementById("refresh-status");
  const select = document.getElementById("refresh-select");
  let busy = false, pending = false, source, scheduled;
  async function refresh() {
    if (busy || document.hidden) {pending = true;return;}
    // Don't move an operator's focus or replace an expanded raw-data table.
    if (section.contains(document.activeElement) || document.querySelector('.toolbar input:focus, .toolbar select:focus') || section.querySelector("details[open]")) {pending = true;status.textContent = "New readings waiting; updates resume when controls or history tables are released.";return;}
    busy = true;pending = false;
    try {
      const response = await fetch(location.href, {credentials: "same-origin", cache: "no-store"});
      if (!response.ok || response.redirected) throw new Error("Session or request unavailable; reload to sign in");
      const doc = new DOMParser().parseFromString(await response.text(), "text/html");
      const next = doc.getElementById("dashboard-data");
      if (!next) throw new Error("Session expired; reload to sign in");
      const oldPanels = new Map([...section.querySelectorAll(".chart-panel")].map(panel => [panelKey(panel), panel]));
      // Reuse chart nodes/instances so zoom, legend selections and listeners survive.
      next.querySelectorAll(".chart-panel").forEach(panel => {
        const old = oldPanels.get(panelKey(panel));
        if (!old) return;
        for (const [key, value] of Object.entries(panel.dataset)) old.dataset[key] = value;
        panel.replaceWith(old);oldPanels.delete(panelKey(old));
      });
      oldPanels.forEach(panel => {if (typeof echarts !== "undefined") echarts.getInstanceByDom(panel.querySelector(".plot"))?.dispose();});
      section.replaceWith(next);section = next;render(section);
      // Newly fetched HTML contains the existing HTMX log controls only outside this section.
      if (typeof htmx !== "undefined") htmx.process(section);
      status.textContent = `Updated ${new Date().toLocaleTimeString()}. Collection runs every 60 seconds.`;
    } catch (error) {status.textContent = `Refresh failed: ${error.message}. Previous readings retained.`;}
    finally {busy = false;if (pending) scheduleRefresh();}
  }
  function scheduleRefresh() {clearTimeout(scheduled);scheduled = setTimeout(refresh, 250);}
  function connect() {
    source?.close();source = undefined;clearTimeout(scheduled);
    if (select.value === "0") {pending = false;status.textContent = "Automatic updates paused.";return;}
    if (typeof EventSource === "undefined") {status.textContent = "Server events unavailable in this browser. Use Refresh now.";return;}
    source = new EventSource("/events");
    source.addEventListener("refresh", event => {
      const update = JSON.parse(event.data);
      if (!section.dataset.host || !update.host || update.host === section.dataset.host) scheduleRefresh();
    });
    source.addEventListener("auth-expired", () => {source.close();status.textContent = "Session expired. Reload to sign in.";});
    source.onopen = () => {status.textContent = "Connected to sample updates. Collection runs every 60 seconds.";};
    source.onerror = () => {status.textContent = "Updates disconnected; reconnecting. Previous readings retained.";};
  }
  select.addEventListener("change", connect);
  document.getElementById("refresh-now").addEventListener("click", refresh);
  document.addEventListener("visibilitychange", () => {if (!document.hidden && select.value !== "0") scheduleRefresh();});
  document.addEventListener("focusout", () => {if (pending && select.value !== "0") scheduleRefresh();});
  document.addEventListener("toggle", () => {if (pending && select.value !== "0") scheduleRefresh();}, true);
  // Observe the stable main container, not chart nodes that may be replaced on refresh.
  const resize = new ResizeObserver(() => {section.querySelectorAll(".plot").forEach(plot => {if (typeof echarts !== "undefined") echarts.getInstanceByDom(plot)?.resize();});});
  resize.observe(document.querySelector("main"));
  window.addEventListener("pagehide", () => {source?.close();clearTimeout(scheduled);resize.disconnect();}, {once: true});
  connect();
}

if (typeof document !== "undefined") initDashboard();
if (typeof module !== "undefined") module.exports = {chartSegments, nearestSample, chartOption};
