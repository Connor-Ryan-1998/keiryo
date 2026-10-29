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
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Docker deliberately exposes only the four read operations this application needs.
type Docker struct {
	client   *http.Client
	base     string
	mu       sync.Mutex
	version  string
	previous map[string]cpuObservation
}

type cpuObservation struct {
	CPU dockerCPU
	At  time.Time
}

func NewDocker(socket string) *Docker {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, MaxConnsPerHost: 2, ResponseHeaderTimeout: 5 * time.Second}
	return &Docker{client: &http.Client{Transport: tr, Timeout: 10 * time.Second}, base: "http://docker"}
}
func (d *Docker) api(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.version != "" {
		return d.version, nil
	}
	var v struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := d.read(ctx, "/version", &v); err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^1\.[0-9]{1,3}$`).MatchString(v.APIVersion) {
		return "", errors.New("invalid Docker API version")
	}
	d.version = "/v" + v.APIVersion
	return d.version, nil
}
func (d *Docker) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := d.client.Do(req)
	if err != nil {
		return nil, errors.New("Docker unavailable")
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, fmt.Errorf("Docker read returned HTTP %d", res.StatusCode)
	}
	return res, nil
}
func (d *Docker) read(ctx context.Context, path string, v any) error {
	res, err := d.get(ctx, path)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(v); err != nil {
		return errors.New("invalid or oversized Docker response")
	}
	return nil
}

type dockerInspect struct {
	RestartCount int
	Config       struct {
		TTY bool `json:"Tty"`
	}
	State struct {
		Status   string
		Health   *struct{ Status string }
		ExitCode *int
	}
}
type dockerCPU struct {
	CPUUsage struct {
		Total uint64 `json:"total_usage"`
	} `json:"cpu_usage"`
	System uint64 `json:"system_cpu_usage"`
	CPUs   uint64 `json:"online_cpus"`
}
type dockerStats struct {
	CPU    dockerCPU `json:"cpu_stats"`
	Memory struct {
		Usage, Limit uint64
		Stats        map[string]uint64
	} `json:"memory_stats"`
}

func (d *Docker) Collect(ctx context.Context) ([]protocol.Container, []protocol.Metric, []string) {
	api, err := d.api(ctx)
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	var list []struct {
		ID    string `json:"Id"`
		Names []string
		State string
	}
	if err = d.read(ctx, api+"/containers/json?all=true", &list); err != nil {
		return nil, nil, []string{err.Error()}
	}
	var containers []protocol.Container
	var metrics []protocol.Metric
	var errs []string
	nextCPU := map[string]cpuObservation{}
	if len(list) > 512 {
		// shortcut: cap collection at 512 containers; expose a JSON limit before larger-host trials.
		list = list[:512]
		errs = append(errs, "Docker container collection truncated at 512")
	}
	for _, c := range list {
		if !config.ValidID(c.ID) {
			continue
		}
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out := protocol.Container{ID: c.ID, Name: name, State: c.State}
		var inspect dockerInspect
		if err = d.read(ctx, api+"/containers/"+c.ID+"/json", &inspect); err != nil {
			if len(errs) < 60 {
				errs = append(errs, "container inspection failed")
			}
		} else {
			out.Restarts = inspect.RestartCount
			if inspect.State.Status != "" {
				out.State = inspect.State.Status
			}
			// Listing and inspection can straddle a restart; only inspection can prove completion.
			if inspect.State.Status == "exited" {
				out.ExitCode = inspect.State.ExitCode
			}
			if inspect.State.Health != nil {
				out.Health = inspect.State.Health.Status
			}
		}
		containers = append(containers, out)
		if out.State != "running" {
			continue
		}
		var stats dockerStats
		if err = d.read(ctx, api+"/containers/"+c.ID+"/stats?stream=false&one-shot=true", &stats); err != nil {
			if len(errs) < 60 {
				errs = append(errs, "container stats unavailable")
			}
			continue
		}
		now := time.Now()
		old, exists := d.previous[c.ID]
		nextCPU[c.ID] = cpuObservation{CPU: stats.CPU, At: now}
		elapsed := now.Sub(old.At).Seconds()
		if exists && elapsed > 0 && elapsed <= 86400 && stats.CPU.System > old.CPU.System && stats.CPU.CPUUsage.Total >= old.CPU.CPUUsage.Total && stats.CPU.CPUs > 0 {
			v := float64(stats.CPU.CPUUsage.Total-old.CPU.CPUUsage.Total) / float64(stats.CPU.System-old.CPU.System) * float64(stats.CPU.CPUs) * 100
			metrics = append(metrics, protocol.Metric{Name: "container_cpu_percent", Entity: c.ID, Value: v, Weight: elapsed})
		}
		mem := stats.Memory.Usage
		if inactive := stats.Memory.Stats["inactive_file"]; inactive <= mem {
			mem -= inactive
		}
		metrics = append(metrics, protocol.Metric{Name: "container_memory_bytes", Entity: c.ID, Value: float64(mem), Weight: 1}, protocol.Metric{Name: "container_memory_limit_bytes", Entity: c.ID, Value: float64(stats.Memory.Limit), Weight: 1})
	}
	d.previous = nextCPU
	return containers, metrics, errs
}
func (d *Docker) Logs(ctx context.Context, id string, lines, limit int) (string, error) {
	if !config.ValidID(id) || lines < 1 || limit < 1 {
		return "", errors.New("invalid log request")
	}
	api, err := d.api(ctx)
	if err != nil {
		return "", err
	}
	var inspect dockerInspect
	if err = d.read(ctx, api+"/containers/"+id+"/json", &inspect); err != nil {
		return "", err
	}
	res, err := d.get(ctx, api+"/containers/"+id+"/logs?stdout=true&stderr=true&timestamps=true&tail="+url.QueryEscape(fmt.Sprint(lines)))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	return decodeLogs(res.Body, inspect.Config.TTY, lines, limit)
}
func decodeLogs(r io.Reader, tty bool, lines, limit int) (string, error) {
	var out strings.Builder
	// Bound framing overhead as well as payload, including malicious zero-length frames.
	r = io.LimitReader(r, int64(limit)+64<<10)
	if tty {
		b, err := io.ReadAll(io.LimitReader(r, int64(limit)))
		if err != nil {
			return "", err
		}
		out.Write(b)
	} else {
		for out.Len() < limit {
			var header [8]byte
			_, err := io.ReadFull(r, header[:])
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", errors.New("truncated Docker log frame")
			}
			if header[0] > 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
				return "", errors.New("invalid Docker log frame")
			}
			n := int64(binary.BigEndian.Uint32(header[4:]))
			take := min(n, int64(limit-out.Len()))
			if _, err = io.CopyN(&out, r, take); err != nil {
				return "", errors.New("truncated Docker log payload")
			}
			if take < n {
				break
			}
		}
	}
	text := out.String()
	count := 0
	for i, c := range text {
		if c == '\n' {
			count++
			if count >= lines {
				return text[:i+1], nil
			}
		}
	}
	return text, nil
}
