package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"houselab/monitoring/internal/config"
	"houselab/monitoring/internal/protocol"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCounterRates(t *testing.T) {
	for _, x := range []struct {
		before, after float64
		elapsed       time.Duration
		want          float64
		ok            bool
	}{{100, 220, 2 * time.Second, 60, true}, {220, 100, time.Second, 0, false}, {0, 100, 0, 0, false}} {
		v, ok := rate(x.before, x.after, x.elapsed)
		if v != x.want || ok != x.ok {
			t.Fatalf("rate %v %v", v, ok)
		}
	}
}
func TestBoundedBufferAndRetry(t *testing.T) {
	b := buffer{max: 2}
	for _, id := range []string{"a", "b", "c"} {
		b.add(protocol.Sample{ID: id})
	}
	if len(b.samples) != 2 || b.samples[0].ID != "b" || b.dropped != 1 {
		t.Fatalf("buffer %+v", b)
	}
	status := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("credential missing")
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	a := &Agent{Config: config.Agent{HubURL: srv.URL}, token: "secret", client: srv.Client()}
	if err := a.uploadState(context.Background(), b.samples[0].State()); err == nil {
		t.Fatal("accepted failed write")
	}
	if b.samples[0].ID != "b" {
		t.Fatal("lost failed sample")
	}
	status = 204
	if err := a.uploadState(context.Background(), b.samples[0].State()); err != nil {
		t.Fatal(err)
	}
	status = 400
	if err := a.uploadState(context.Background(), b.samples[0].State()); !errors.Is(err, errRejected) {
		t.Fatalf("permanent rejection treated as retryable: %v", err)
	}
}
func TestDockerLogDecodeBounded(t *testing.T) {
	var frame strings.Builder
	payload := "<script>alert(1)</script>\nnext\n"
	head := make([]byte, 8)
	head[0] = 1
	binary.BigEndian.PutUint32(head[4:], uint32(len(payload)))
	frame.Write(head)
	frame.WriteString(payload)
	got, err := decodeLogs(strings.NewReader(frame.String()), false, 200, 4096)
	if err != nil || got != payload {
		t.Fatalf("decode %q %v", got, err)
	}
	got, err = decodeLogs(strings.NewReader("one\ntwo\nthree\n"), true, 2, 5)
	if err != nil || len(got) > 5 {
		t.Fatalf("unbounded logs %q %v", got, err)
	}
	if _, err = decodeLogs(strings.NewReader("bad"), false, 10, 100); err == nil {
		t.Fatal("accepted truncated frame")
	}
}
func TestDockerReadOnlyEndpoints(t *testing.T) {
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("Docker mutation attempted")
		}
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"ApiVersion":"1.47"}`)
		case "/v1.47/containers/json":
			io.WriteString(w, `[{"Id":"abc","Names":["/test"],"State":"running"}]`)
		case "/v1.47/containers/abc/json":
			io.WriteString(w, `{"RestartCount":2,"Config":{"Tty":true},"State":{"Health":{"Status":"healthy"}}}`)
		case "/v1.47/containers/abc/stats":
			reads++
			io.WriteString(w, fmt.Sprintf(`{"cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d,"online_cpus":2},"precpu_stats":{},"memory_stats":{"usage":300,"limit":1000,"stats":{"inactive_file":100}}}`, 100+reads*100, 500+reads*500))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := &Docker{client: srv.Client(), base: srv.URL}
	containers, metrics, errs := d.Collect(context.Background())
	if len(errs) != 0 || len(containers) != 1 || containers[0].Restarts != 2 || len(metrics) != 2 {
		t.Fatalf("collect %v %v %v", containers, metrics, errs)
	}
	_, metrics, errs = d.Collect(context.Background())
	if len(errs) != 0 || len(metrics) != 3 {
		t.Fatalf("second observation %v %v", metrics, errs)
	}
	if metrics[0].Value != 40 || metrics[1].Value != 200 {
		t.Fatalf("CPU/memory definitions wrong: %+v", metrics)
	}
}

func TestDockerCollectExitCodesWithoutInventingSuccess(t *testing.T) {
	for _, tc := range []struct {
		inspect, state string
		exit           int
	}{
		{`{"Status":"exited","ExitCode":0}`, "exited", 0},
		{`{"Status":"exited","ExitCode":7}`, "exited", 7},
		{`{"Status":"exited"}`, "exited", -1},
		{`{}`, "exited", -1},
		{`{"ExitCode":0}`, "exited", -1},
		{`{"Status":"running","ExitCode":0}`, "running", -1},
		{`{"Status":"created","ExitCode":0}`, "created", -1},
	} {
		t.Run(tc.inspect, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/version":
					io.WriteString(w, `{"ApiVersion":"1.47"}`)
				case "/v1.47/containers/json":
					io.WriteString(w, `[{"Id":"job","Names":["/init"],"State":"exited"}]`)
				case "/v1.47/containers/job/json":
					io.WriteString(w, `{"State":`+tc.inspect+`}`)
				case "/v1.47/containers/job/stats":
					io.WriteString(w, `{}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			d := &Docker{client: srv.Client(), base: srv.URL}
			containers, _, errs := d.Collect(context.Background())
			if len(errs) != 0 || len(containers) != 1 {
				t.Fatalf("collect: %v %v", containers, errs)
			}
			data, err := json.Marshal(containers[0])
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got["state"] != tc.state {
				t.Errorf("state=%v, want %s", got["state"], tc.state)
			}
			if tc.exit < 0 {
				if _, exists := got["exit_code"]; exists {
					t.Fatal("invented exit code for missing inspection field")
				}
			} else {
				if got["exit_code"] != float64(tc.exit) {
					t.Fatalf("missing/wrong exit code: %s", data)
				}
			}
		})
	}
}

func TestRejectedSampleDoesNotBlockNewSamples(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ids := make(chan string, 10)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(200)
			return
		}
		if r.Method != "POST" {
			w.WriteHeader(204)
			return
		}
		var s protocol.Sample
		json.NewDecoder(r.Body).Decode(&s)
		ids <- s.ID
		if attempts.Add(1) == 1 {
			w.WriteHeader(400)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	c := config.DefaultAgent()
	c.HostID = "node"
	c.HubURL = srv.URL
	c.IntervalSeconds = 1
	c.PollSeconds = 60
	collector := NewCollector(c)
	collector.Docker = nil
	a := &Agent{Config: c, token: "secret", client: srv.Client(), collector: collector}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var first string
	select {
	case first = <-ids:
	case <-ctx.Done():
		t.Fatal("first upload missing")
	}
	select {
	case next := <-ids:
		if next == first {
			t.Fatal("permanently rejected sample retried instead of progressing")
		}
	case <-ctx.Done():
		t.Fatal("new samples blocked")
	}
	cancel()
	<-done
}
