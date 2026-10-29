// Package config loads portable JSON configuration. Durations are explicit seconds.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"
)

type Hub struct {
	UIListen              string            `json:"ui_listen"`
	AgentListen           string            `json:"agent_listen"`
	Database              string            `json:"database"`
	AuthMode              string            `json:"auth_mode"`
	TrustedProxies        []string          `json:"trusted_proxies"`
	PublicURL             string            `json:"public_url"`
	Agents                map[string]string `json:"agents"`
	RawHours              int               `json:"raw_hours"`
	SummaryDays           int               `json:"summary_days"`
	BucketSeconds         int               `json:"bucket_seconds"`
	StorageBytes          int64             `json:"storage_bytes"`
	StaleSeconds          int               `json:"stale_seconds"`
	OfflineSeconds        int               `json:"offline_seconds"`
	FutureSeconds         int               `json:"future_seconds"`
	MaxBodyBytes          int64             `json:"max_body_bytes"`
	MaxEntities           int               `json:"max_entities"`
	MaxRequests           int               `json:"max_requests"`
	LogLines              int               `json:"log_lines"`
	LogBytes              int               `json:"log_bytes"`
	LogTimeoutSeconds     int               `json:"log_timeout_seconds"`
	LogQueue              int               `json:"log_queue"`
	DiskHealthMountpoints []string          `json:"disk_health_mountpoints"`
	AlertPolicy           AlertPolicy       `json:"alert_policy"`
}

type Agent struct {
	HostID             string   `json:"host_id"`
	DisplayName        string   `json:"display_name"`
	HubURL             string   `json:"hub_url"`
	OTLPMetricsURL     string   `json:"otlp_metrics_url,omitempty"`
	CredentialFile     string   `json:"credential_file"`
	IntervalSeconds    int      `json:"interval_seconds"`
	BufferSamples      int      `json:"buffer_samples"`
	PollSeconds        int      `json:"poll_seconds"`
	DockerSocket       string   `json:"docker_socket"`
	ExcludeInterfaces  []string `json:"exclude_interfaces"`
	ExcludeFilesystems []string `json:"exclude_filesystems"`
	LogBytes           int      `json:"log_bytes"`
	LogLines           int      `json:"log_lines"`
	LogTimeoutSeconds  int      `json:"log_timeout_seconds"`
}

func DefaultHub() Hub {
	return Hub{UIListen: "127.0.0.1:9080", AgentListen: "127.0.0.1:9081", Database: "monitoring.db", AuthMode: "local", PublicURL: "http://127.0.0.1:9080", Agents: map[string]string{}, RawHours: 24, SummaryDays: 30, BucketSeconds: 300, StorageBytes: 1 << 30, StaleSeconds: 180, OfflineSeconds: 300, FutureSeconds: 300, MaxBodyBytes: 2 << 20, MaxEntities: 2048, MaxRequests: 32, LogLines: 200, LogBytes: 256 << 10, LogTimeoutSeconds: 15, LogQueue: 32, AlertPolicy: defaultAlertPolicy()}
}
func DefaultAgent() Agent {
	return Agent{IntervalSeconds: 60, BufferSamples: 60, PollSeconds: 5, DockerSocket: "/var/run/docker.sock", ExcludeInterfaces: []string{"lo"}, LogBytes: 256 << 10, LogLines: 200, LogTimeoutSeconds: 15}
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func ValidID(s string) bool { return identifier.MatchString(s) }

func decode(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	data = bytes.TrimSpace(data)
	if err != nil || len(data) == 0 || len(data) > 1<<20 || data[0] != '{' {
		return errors.New("configuration must be a bounded JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return errors.New("invalid JSON configuration or unknown field")
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return errors.New("configuration must contain one JSON object")
	}
	return nil
}
func relative(configPath, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	abs, _ := filepath.Abs(filepath.Join(filepath.Dir(configPath), p))
	return abs
}

func LoadHub(path string) (Hub, error) {
	c := DefaultHub()
	if err := decode(path, &c); err != nil {
		return c, err
	}
	if err := c.AlertPolicy.Validate(); err != nil {
		return c, err
	}
	for _, mount := range c.DiskHealthMountpoints {
		if !pathpkg.IsAbs(mount) || pathpkg.Clean(mount) != mount {
			return c, errors.New("disk_health_mountpoints requires clean absolute mountpoints")
		}
	}
	if c.RawHours < 1 || c.RawHours > 8760 || c.SummaryDays < 1 || c.SummaryDays > 3650 || c.SummaryDays*24 < c.RawHours || c.BucketSeconds < 60 || c.BucketSeconds > 3600 || c.RawHours*3600%c.BucketSeconds != 0 {
		return c, errors.New("invalid retention durations")
	}
	if c.StorageBytes < 1<<20 || c.StaleSeconds < 1 || c.OfflineSeconds <= c.StaleSeconds || c.FutureSeconds < 0 || c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 16<<20 || c.MaxEntities < 1 || c.MaxEntities > 10000 || c.MaxRequests < 1 || c.MaxRequests > 1024 || c.LogLines < 1 || c.LogLines > 10000 || c.LogBytes < 1 || c.LogBytes > 4<<20 || c.LogTimeoutSeconds < 1 || c.LogTimeoutSeconds > 60 || c.LogQueue < 1 || c.LogQueue > 1024 {
		return c, errors.New("invalid resource or freshness limits")
	}
	if c.AuthMode != "local" && c.AuthMode != "proxy" {
		return c, errors.New("auth_mode must be local or proxy")
	}
	for _, addr := range []string{c.UIListen, c.AgentListen} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return c, errors.New("invalid listen address")
		}
	}
	if c.UIListen == c.AgentListen || c.Database == "" {
		return c, errors.New("separate listeners and database are required")
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && Loopback(u.Hostname()))) {
		return c, errors.New("public_url requires HTTPS or loopback HTTP")
	}
	for _, p := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			return c, errors.New("invalid trusted_proxies CIDR")
		}
	}
	if c.AuthMode == "proxy" && (len(c.TrustedProxies) == 0 || u.Scheme != "https") {
		return c, errors.New("proxy mode requires trusted_proxies and HTTPS public_url")
	}
	c.Database = relative(path, c.Database)
	for id, p := range c.Agents {
		if !ValidID(id) || p == "" {
			return c, errors.New("invalid agent ID or credential path")
		}
		c.Agents[id] = relative(path, p)
	}
	return c, nil
}

func LoadAgent(path string) (Agent, error) {
	c := DefaultAgent()
	if err := decode(path, &c); err != nil {
		return c, err
	}
	if !ValidID(c.HostID) || len(c.DisplayName) > 256 || c.CredentialFile == "" {
		return c, errors.New("host_id and credential_file are required; display_name must be bounded")
	}
	u, err := url.Parse(c.HubURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return c, errors.New("hub_url requires HTTP(S) origin")
	}
	if c.IntervalSeconds < 1 || c.IntervalSeconds > 86400 || c.BufferSamples < 1 || c.BufferSamples > 1440 || c.PollSeconds < 1 || c.PollSeconds > 60 || c.LogBytes < 1 || c.LogBytes > 4<<20 || c.LogLines < 1 || c.LogLines > 10000 || c.LogTimeoutSeconds < 1 || c.LogTimeoutSeconds > 60 {
		return c, errors.New("invalid agent resource limits")
	}
	if c.DockerSocket == "" {
		return c, errors.New("docker_socket is required")
	}
	c.CredentialFile = relative(path, c.CredentialFile)
	c.DockerSocket = relative(path, c.DockerSocket)
	c.HubURL = strings.TrimRight(c.HubURL, "/")
	endpoint, err := url.Parse(c.MetricsURL())
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return c, errors.New("otlp_metrics_url requires a full HTTP(S) URL without credentials or query/fragment")
	}
	return c, nil
}

func (c Agent) MetricsURL() string {
	if c.OTLPMetricsURL != "" {
		return c.OTLPMetricsURL
	}
	return strings.TrimRight(c.HubURL, "/") + "/v1/metrics"
}

func Loopback(host string) bool { ip := net.ParseIP(host); return ip != nil && ip.IsLoopback() }

// ReadSecret refuses world/group-readable files and never includes their contents in errors.
func ReadSecret(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open credential file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("credential file must be regular and mode 0600 or stricter")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", errors.New("invalid credential file")
	}
	s := strings.TrimSpace(string(b))
	if len(s) < 32 || strings.ContainsAny(s, "\r\n\t ") {
		return "", errors.New("credential must contain at least 32 non-whitespace characters")
	}
	return s, nil
}
