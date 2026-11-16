const {test} = require("node:test");
const assert = require("node:assert/strict");
const {chartSegments, nearestSample, chartOption, formatValue, problemSince, relativeTime} = require("../../internal/hub/static/dashboard.js");

test("injected thresholds control chart markers and capacity colours", () => {
  const thresholds = {warning:60,critical:75};
  const line = chartOption({percent:true,thresholds,start:0,end:10,gap:5,series:[{Name:"CPU",Samples:[{At:1,Value:80}]}]});
  assert.deepEqual(line.series[0].markLine.data.map(p => p.yAxis), [60,75]);
  const tree = chartOption({kind:"treemap",thresholds,series:[{Name:"node",Value:1024,Used:80}]});
  assert.equal(tree.series[0].data[0].itemStyle.color,"#f2495c");
  const heat = chartOption({kind:"heatmap",thresholds,start:0,end:10,step:5,series:[]});
  assert.deepEqual(heat.visualMap.pieces.map(p => [p.gte,p.lt]), [[0,60],[60,75],[75,undefined]]);
});

test("gaps stay disconnected and singleton samples remain visible", () => {
  const samples = [{At: 0, Value: 0}, {At: 60, Value: 25}, {At: 180, Value: 80}];
  assert.deepEqual(chartSegments(samples, 0, 240, 90), [samples.slice(0, 2), samples.slice(2)]);
  assert.equal(nearestSample(samples, 120, 90), null);
  assert.deepEqual(nearestSample(samples, 65, 90), samples[1]);
  assert.deepEqual(chartSegments([], 0, 240, 90), []);
  assert.equal(nearestSample([], 100, 90), null);
});

test("ECharts line data keeps outages disconnected and percent thresholds host-normalized", () => {
  const input = {kind:"line",unit:"%",percent:true,start:0,end:240,gap:90,series:[{Name:"CPU",Samples:[{At:0,Value:0},{At:60,Value:95},{At:180,Value:30}]}]};
  const option = chartOption(input);
  assert.equal(option.series[0].connectNulls, false);
  assert.ok(option.series[0].data.some(p => p[1] === null));
  assert.equal(option.series[0].data[0][1], 0);
  assert.deepEqual(option.series[0].markLine.data.map(p => p.yAxis), [80,90]);
  const container = chartOption({...input,percent:false});
  assert.equal(container.series[0].markLine, undefined);
  assert.ok(container.series[0].data.some(p => p[1] === 95));
  assert.equal(option.tooltip.renderMode, "richText");
  assert.equal(option.xAxis.axisLabel.color, "#b8b8ba");
  assert.equal(option.yAxis.axisLabel.color, "#b8b8ba");
});

test("heatmap shows peaks without filling missing cells and treemap uses RAM size", () => {
  const heat = chartOption({kind:"heatmap",unit:"%",start:0,end:3000,step:1000,series:[{Host:"node",At:0,Value:25,Peak:95}]});
  assert.equal(heat.xAxis.data.length,3);
  assert.equal(heat.series[0].data.length,1);
  assert.deepEqual(heat.series[0].data[0].slice(0,3),[0,0,95]);
  const tree = chartOption({kind:"treemap",series:[{Name:"<unsafe>",Value:1024,Used:85}]});
  assert.equal(tree.series[0].data[0].name,"<unsafe>");
  assert.equal(tree.series[0].data[0].value,1024);
  assert.equal(tree.series[0].data[0].itemStyle.color,"#ff9830");
});

test("all chart kinds retain dark tooltips with readable text", () => {
  for (const kind of ["line", "heatmap", "treemap"]) {
    const option = chartOption({kind, start:0, end:3000, step:1000, series:[]});
    assert.equal(option.tooltip.backgroundColor, "#111217");
    assert.equal(option.tooltip.textStyle.color, "#ccccdc");
    assert.equal(option.tooltip.renderMode, "richText");
  }
});

test("sparkline singleton segments have visible markers without marking every point", () => {
  const option = chartOption({kind:"spark",start:0,end:300,gap:90,series:[{Name:"CPU",Samples:[{At:0,Value:25},{At:60,Value:30},{At:240,Value:10}]}]});
  const line = option.series[0];
  assert.equal(line.showSymbol,true);
  assert.equal(line.symbolSize([240,10]),4);
  assert.equal(line.symbolSize([60,30]),0);
});

test("pixel reduction preserves endpoints and peaks without joining outages", () => {
  const samples = Array.from({length: 10000}, (_, At) => ({At, Value: At === 5005 ? 100 : At === 5006 ? 0 : 50}));
  const [reduced] = chartSegments(samples, 0, 10000, 2);
  assert.ok(reduced.length <= 2404);
  assert.deepEqual(reduced[0], samples[0]);
  assert.deepEqual(reduced.at(-1), samples.at(-1));
  assert.ok(reduced.some(p => p.Value === 100));
  assert.ok(reduced.some(p => p.Value === 0));
  assert.equal(chartSegments([...samples, {At: 20000, Value: 25}], 0, 21000, 2).length, 2);
});

test("problemSince walks back to the first over-threshold sample", () => {
  assert.deepEqual(problemSince([{At:0,Value:50},{At:60,Value:92},{At:120,Value:95}], 90, 90, 0), {kind:"since",at:60});
  assert.deepEqual(problemSince([{At:0,Value:95},{At:60,Value:96}], 90, 90, 0), {kind:"before"});
  assert.deepEqual(problemSince([{At:0,Value:95},{At:60,Value:70}], 90, 90, 0), {kind:"latest"});
  assert.deepEqual(problemSince([{At:0,Value:95},{At:500,Value:95}], 90, 90, 0), {kind:"since",at:500});
  assert.deepEqual(problemSince([], 90, 90, 0), {kind:"latest"});
});

test("byte rates scale units like the server", () => {
  assert.equal(formatValue(512, "B/s"), "512 B/s");
  assert.equal(formatValue(3072, "B/s"), "3.0 KiB/s");
  assert.equal(formatValue(1.5 * 2 ** 20, "B/s"), "1.5 MiB/s");
  assert.equal(formatValue(25, "%"), "25 %");
});

test("relative times use coarse buckets", () => {
  const now = 10 ** 10;
  assert.equal(relativeTime(now - 18000, now), "18s ago");
  assert.equal(relativeTime(now - 300000, now), "5m ago");
  assert.equal(relativeTime(now - 3 * 3600000, now), "3h ago");
  assert.equal(relativeTime(now - 2 * 86400000, now), "2d ago");
  assert.equal(relativeTime(now + 5000, now), "just now");
});

test("threshold labels and heatmap edges have room", () => {
  const line = chartOption({percent:true,start:0,end:10,gap:5,series:[{Name:"CPU",Samples:[{At:1,Value:80}]}]});
  assert.ok(line.grid.right >= 45);
  const heat = chartOption({kind:"heatmap",start:0,end:3000,step:1000,series:[]});
  assert.ok(heat.grid.right >= 45);
});

test("byte-rate axes use scaled units", () => {
  const option = chartOption({unit:"B/s",start:0,end:10,gap:5,series:[{Name:"eth0 receive",Samples:[{At:1,Value:2048}]}]});
  assert.equal(option.yAxis.axisLabel.formatter(2048), "2.0 KiB/s");
});
