package hub

import (
	"bytes"
	"compress/gzip"
	"context"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"houselab/monitoring/internal/otlp"
	"houselab/monitoring/internal/protocol"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func otlpBody(t *testing.T, host string) *collectorpb.ExportMetricsServiceRequest {
	t.Helper()
	req, err := otlp.EncodeSample(protocol.Sample{HostID: host, At: time.Now().UTC(), Metrics: []protocol.Metric{{Name: "uptime_seconds", Value: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	return req
}
func postOTLP(h *Hub, body []byte, content, encoding, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/metrics", bytes.NewReader(body))
	r.Header.Set("Content-Type", content)
	r.Header.Set("Content-Encoding", encoding)
	r.Header.Set("X-Host-ID", "node")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.AgentHandler().ServeHTTP(w, r)
	return w
}
func TestOTLPWireFormatsGzipAndPartialSuccess(t *testing.T) {
	for _, content := range []string{"application/x-protobuf", "application/json"} {
		for _, compressed := range []bool{false, true} {
			h := testHub(t, "local")
			req := otlpBody(t, "node")
			unknown := proto.Clone(req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0]).(*metricpb.Metric)
			unknown.Name = "application.unsupported"
			req.ResourceMetrics[0].ScopeMetrics[0].Metrics = append(req.ResourceMetrics[0].ScopeMetrics[0].Metrics, unknown)
			var body []byte
			if content == "application/json" {
				body, _ = protojson.Marshal(req)
			} else {
				body, _ = proto.Marshal(req)
			}
			encoding := ""
			if compressed {
				var b bytes.Buffer
				g := gzip.NewWriter(&b)
				g.Write(body)
				g.Close()
				body = b.Bytes()
				encoding = "gzip"
			}
			w := postOTLP(h, body, content, encoding, strings.Repeat("s", 32))
			if w.Code != 200 || w.Header().Get("Content-Type") != content {
				t.Fatalf("%s %v: %d %s", content, compressed, w.Code, w.Body.String())
			}
			out := new(collectorpb.ExportMetricsServiceResponse)
			var err error
			if content == "application/json" {
				err = protojson.Unmarshal(w.Body.Bytes(), out)
			} else {
				err = proto.Unmarshal(w.Body.Bytes(), out)
			}
			if err != nil || out.GetPartialSuccess().GetRejectedDataPoints() != 1 {
				t.Fatal(out, err)
			}
			host, _ := h.store.Host(context.Background(), "node")
			if len(host.Sample.Metrics) != 1 || host.Sample.Metrics[0].Value != 100 {
				t.Fatal(host)
			}
		}
	}
}
func TestOTLPErrorsAreEncodedAndDoNotIngest(t *testing.T) {
	for _, tc := range []struct {
		name, token, content, encoding string
		body                           []byte
		want                           int
	}{
		{name: "unauthorized", content: "application/x-protobuf", want: 401},
		{name: "bad json", token: strings.Repeat("s", 32), content: "application/json", body: []byte("bad"), want: 400},
		{name: "unsupported media", token: strings.Repeat("s", 32), content: "text/plain", want: 415},
		{name: "unsupported encoding", token: strings.Repeat("s", 32), content: "application/x-protobuf", encoding: "br", want: 415},
		{name: "bad gzip", token: strings.Repeat("s", 32), content: "application/x-protobuf", encoding: "gzip", body: []byte("bad"), want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testHub(t, "local")
			w := postOTLP(h, tc.body, tc.content, tc.encoding, tc.token)
			if w.Code != tc.want {
				t.Fatalf("%d want %d", w.Code, tc.want)
			}
			s := new(statuspb.Status)
			var err error
			if tc.content == "application/json" {
				err = protojson.Unmarshal(w.Body.Bytes(), s)
			} else {
				err = proto.Unmarshal(w.Body.Bytes(), s)
			}
			if err != nil || s.Message == "" {
				t.Fatal(s, err)
			}
			host, _ := h.store.Host(context.Background(), "node")
			if !host.Seen.IsZero() {
				t.Fatal("failed request changed contact")
			}
		})
	}
	h := testHub(t, "local")
	b, _ := proto.Marshal(otlpBody(t, "foreign"))
	if w := postOTLP(h, b, "application/x-protobuf", "", strings.Repeat("s", 32)); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestOTLPBoundsAndEmptyExports(t *testing.T) {
	h := testHub(t, "local")
	h.config.MaxBodyBytes = 1024
	var b bytes.Buffer
	g := gzip.NewWriter(&b)
	io.WriteString(g, strings.Repeat(" ", 2048))
	g.Close()
	if w := postOTLP(h, b.Bytes(), "application/json", "gzip", strings.Repeat("s", 32)); w.Code != 413 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := postOTLP(h, []byte(strings.Repeat(" ", 2048)), "application/json", "", strings.Repeat("s", 32)); w.Code != 413 {
		t.Fatal(w.Code)
	}
	if w := postOTLP(h, nil, "application/x-protobuf", "", strings.Repeat("s", 32)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	host, _ := h.store.Host(context.Background(), "node")
	if !host.Seen.IsZero() {
		t.Fatal("empty export invented contact")
	}
	h.slots <- struct{}{}
	for len(h.slots) < cap(h.slots) {
		h.slots <- struct{}{}
	}
	if w := postOTLP(h, nil, "application/x-protobuf", "", strings.Repeat("s", 32)); w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Code)
	}
	for len(h.slots) > 0 {
		<-h.slots
	}
	h.config.MaxEntities = 1
	req := otlpBody(t, "node")
	req.ResourceMetrics = append(req.ResourceMetrics, proto.Clone(req.ResourceMetrics[0]).(*metricpb.ResourceMetrics))
	body, _ := proto.Marshal(req)
	if w := postOTLP(h, body, "application/x-protobuf", "", strings.Repeat("s", 32)); w.Code != 400 {
		t.Fatal(w.Code)
	}
}
func TestOTLPStorageFailureIsRetryable(t *testing.T) {
	h := testHub(t, "local")
	h.store.Close()
	body, _ := proto.Marshal(otlpBody(t, "node"))
	w := postOTLP(h, body, "application/x-protobuf", "", strings.Repeat("s", 32))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
}
