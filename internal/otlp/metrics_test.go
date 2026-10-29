package otlp

import (
	"math"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func attr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}
func export(name, unit string, value float64, attrs ...*commonpb.KeyValue) *collectorpb.ExportMetricsServiceRequest {
	return &collectorpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{attr("host.id", "node")}}, ScopeMetrics: []*metricpb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: "fixture"}, Metrics: []*metricpb.Metric{{Name: name, Unit: unit, Data: &metricpb.Metric_Gauge{Gauge: &metricpb.Gauge{DataPoints: []*metricpb.NumberDataPoint{{TimeUnixNano: uint64(time.Now().UnixNano()), Value: &metricpb.NumberDataPoint_AsDouble{AsDouble: value}, Attributes: attrs}}}}}}}}}}}
}
func normalize(t *testing.T, req *collectorpb.ExportMetricsServiceRequest) ([]Point, int64, error) {
	t.Helper()
	return Normalize(req, "node", time.Now(), 24*time.Hour, 5*time.Minute, 100)
}
func TestDecodeBothEncodingsAndUnknownFields(t *testing.T) {
	req := export("system.uptime", "s", 120)
	binary, _ := proto.Marshal(req)
	json, _ := protojson.Marshal(req)
	json = append([]byte(`{"futureField":true,`), json[1:]...)
	for _, tc := range []struct {
		content string
		body    []byte
	}{{"application/x-protobuf", binary}, {"application/json", json}} {
		got, err := Decode(tc.body, tc.content)
		if err != nil {
			t.Fatal(err)
		}
		points, rejected, err := normalize(t, got)
		if err != nil || rejected != 0 || len(points) != 1 || points[0].Value != 120 {
			t.Fatalf("%+v %d %v", points, rejected, err)
		}
	}
	if _, err := Decode([]byte("bad"), "application/json"); err == nil {
		t.Fatal("malformed request accepted")
	}
}
func TestNormalizeIdentityAndLimits(t *testing.T) {
	a := export("system.filesystem.usage", "By", 100, attr("mountpoint", "/"), attr("state", "free"))
	p, _, err := normalize(t, a)
	if err != nil || len(p) != 1 {
		t.Fatal(p, err)
	}
	b := proto.Clone(a).(*collectorpb.ExportMetricsServiceRequest)
	dp := b.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0]
	dp.Attributes[0], dp.Attributes[1] = dp.Attributes[1], dp.Attributes[0]
	q, _, _ := normalize(t, b)
	if p[0].Series != q[0].Series {
		t.Fatal("attribute order changed identity")
	}
	b.ResourceMetrics[0].Resource.Attributes[0] = attr("host.id", "foreign")
	if _, _, err := normalize(t, b); err == nil {
		t.Fatal("cross-host identity accepted")
	}
	b.ResourceMetrics[0].Resource.Attributes = nil
	if _, _, err := normalize(t, b); err == nil {
		t.Fatal("missing host identity accepted")
	}
	a.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0].Attributes = append(dp.Attributes, attr("state", "used"))
	if _, _, err := normalize(t, a); err == nil {
		t.Fatal("duplicate attribute accepted")
	}
}
func TestNormalizeRejectsUnsupportedPoints(t *testing.T) {
	for _, tc := range []struct {
		name, unit string
		value      float64
	}{{"unknown", "1", 1}, {"system.uptime", "By", 1}, {"system.uptime", "s", math.NaN()}, {"system.uptime", "s", -1}, {"container.memory.usage.total", "By", 1}} {
		p, n, err := normalize(t, export(tc.name, tc.unit, tc.value))
		if err != nil || len(p) != 0 || n != 1 {
			t.Fatalf("%s: %+v %d %v", tc.name, p, n, err)
		}
	}
	req := export("system.uptime", "s", 1)
	req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Data = &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{DataPoints: []*metricpb.HistogramDataPoint{{}, {}}}}
	_, n, err := normalize(t, req)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
}
func TestNormalizeMultipleResourcesAndTimestamps(t *testing.T) {
	a := export("system.uptime", "s", 10)
	b := export("system.memory.limit", "By", 1024)
	a.ResourceMetrics = append(a.ResourceMetrics, b.ResourceMetrics...)
	p, n, err := normalize(t, a)
	if err != nil || n != 0 || len(p) != 2 || p[0].At.Equal(p[1].At) {
		t.Fatal(p, n, err)
	}
	a.ResourceMetrics[1].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0].TimeUnixNano = 1
	if _, n, err := normalize(t, a); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, _, err := Normalize(a, "node", time.Now(), time.Hour, time.Minute, 1); err == nil {
		t.Fatal("point limit ignored")
	}
}

func TestProjectCollectorUnitsAndDefinitions(t *testing.T) {
	at := time.Now().UTC()
	points := []Point{
		{Name: "container.cpu.utilization", Value: 150, Entity: "abc", At: at, Group: "docker"},
		{Name: "system.memory.limit", Value: 1000, At: at, Group: "memory"},
		{Name: "system.linux.memory.available", Value: 400, At: at, Group: "memory"},
		{Name: "system.filesystem.usage", Value: 200, Entity: "/", Attributes: map[string]string{"state": "free"}, At: at, Group: "fs"},
		{Name: "system.filesystem.usage", Value: 700, Entity: "/", Attributes: map[string]string{"state": "used"}, At: at, Group: "fs"},
		{Name: "system.filesystem.usage", Value: 100, Entity: "/", Attributes: map[string]string{"state": "reserved"}, At: at, Group: "fs"},
	}
	got := map[string]float64{}
	for _, p := range Project(points) {
		got[p.Metric.Name] = p.Metric.Value
	}
	for name, want := range map[string]float64{"container_cpu_percent": 150, "memory_total_bytes": 1000, "memory_used_bytes": 600, "memory_available_bytes": 400, "filesystem_total_bytes": 1000, "filesystem_free_bytes": 200} {
		if got[name] != want {
			t.Fatalf("%s: %v want %v", name, got[name], want)
		}
	}
	points[2].At = at.Add(-time.Minute)
	for _, p := range Project(points) {
		if p.Metric.Name == "memory_used_bytes" {
			t.Fatal("paired mismatched memory times")
		}
	}
}

func TestCPUUtilizationIdleDoesNotRequireBusyStates(t *testing.T) {
	p := Point{Name: "system.cpu.utilization", At: time.Now(), Group: "cpu", Value: .25, Attributes: map[string]string{"cpu": "all", "state": "idle"}}
	got := Project([]Point{p})
	if len(got) != 1 || got[0].Metric.Value != 75 {
		t.Fatal(got)
	}
}

func TestAggregatedCollectorCPUAndNonAggregatedDimensions(t *testing.T) {
	req := export("system.cpu.utilization", "1", .25, attr("state", "idle"))
	points, n, err := normalize(t, req)
	if err != nil || n != 0 || len(points) != 1 {
		t.Fatal("default aggregated Collector CPU rejected", points, n, err)
	}
	if got := Project(points); len(got) != 1 || got[0].Metric.Value != 75 {
		t.Fatal(got)
	}
	a := export("system.filesystem.usage", "By", 100, attr("mountpoint", "/"), attr("state", "free"), attr("device", "/dev/a"))
	b := proto.Clone(a).(*collectorpb.ExportMetricsServiceRequest)
	b.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints[0].Attributes[2] = attr("device", "/dev/b")
	p, _, _ := normalize(t, a)
	q, _, _ := normalize(t, b)
	if p[0].Group == q[0].Group {
		t.Fatal("different filesystem devices merged into one projection")
	}
}

func TestCumulativeCPURequiresCompleteMatchingIntervals(t *testing.T) {
	at := time.Now().UTC()
	points := []Point{}
	for _, state := range []string{"idle", "wait", "user", "system", "nice", "interrupt", "softirq", "steal"} {
		rate := 0.0
		if state == "idle" {
			rate = .8
		}
		if state == "user" {
			rate = .2
		}
		points = append(points, Point{Name: "system.cpu.time", Group: "cpu", At: at, Start: at.Add(-10 * time.Second), Counter: true, Rate: &rate, Attributes: map[string]string{"cpu": "0", "state": state}})
	}
	if got := Project(points[:1]); len(got) != 0 {
		t.Fatal("incomplete cumulative CPU became a reading", got)
	}
	if got := Project(points); len(got) != 1 || math.Abs(got[0].Metric.Value-20) > 1e-9 {
		t.Fatal(got)
	}
	points[2].Start = at.Add(-20 * time.Second)
	if got := Project(points); len(got) != 0 {
		t.Fatal("incompatible rate intervals combined", got)
	}
}
