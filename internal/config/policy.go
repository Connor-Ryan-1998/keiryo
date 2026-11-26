package config

import (
	"errors"
	"math"
	"strings"
)

type Thresholds struct {
	Warning  float64 `json:"warning"`
	Critical float64 `json:"critical"`
}

func (t Thresholds) State(v *float64) string {
	if v == nil {
		return "unavailable"
	}
	if *v >= t.Critical {
		return "critical"
	}
	if *v >= t.Warning {
		return "warning"
	}
	return "healthy"
}

type ResourcePolicy struct {
	CPU    Thresholds `json:"cpu"`
	Memory Thresholds `json:"memory"`
	Disk   DiskPolicy `json:"disk"`
}

// DiskPolicy adds a fill forecast to the usual disk thresholds.
type DiskPolicy struct {
	Thresholds
	Forecast DiskForecast `json:"forecast"`
}

// DiskForecast warns when a linear fit of recent usage reaches capacity within
// Days. Days 0 disables it.
type DiskForecast struct {
	Days        float64 `json:"days"`
	WindowHours int     `json:"window_hours"`
	Severity    string  `json:"severity"`
}

type ContainerPolicy struct {
	Stopped     string      `json:"stopped"`
	Unhealthy   string      `json:"unhealthy"`
	Starting    string      `json:"starting"`
	RestartLoop RestartLoop `json:"restart_loop"`
}

// RestartLoop flags a container whose Docker restart count rises by at least
// Restarts within WindowSeconds. Restarts 0 disables the check. In an override,
// a zero window or empty severity inherits the global value.
type RestartLoop struct {
	Restarts      int    `json:"restarts"`
	WindowSeconds int    `json:"window_seconds,omitempty"`
	Severity      string `json:"severity,omitempty"`
}

type ContainerOverride struct {
	Host        string       `json:"host"`
	Name        string       `json:"name"`
	Lifecycle   string       `json:"lifecycle,omitempty"`
	Failure     string       `json:"failure,omitempty"`
	RestartLoop *RestartLoop `json:"restart_loop,omitempty"`
}

// RestartLoopFor returns the effective restart-loop rule for one container.
func (p AlertPolicy) RestartLoopFor(host, name string) RestartLoop {
	rule := p.Containers.RestartLoop
	for _, o := range p.ContainerOverrides {
		if o.Host == host && o.Name == name && o.RestartLoop != nil {
			rule.Restarts = o.RestartLoop.Restarts
			if o.RestartLoop.WindowSeconds != 0 {
				rule.WindowSeconds = o.RestartLoop.WindowSeconds
			}
			if o.RestartLoop.Severity != "" {
				rule.Severity = o.RestartLoop.Severity
			}
		}
	}
	return rule
}

type AlertPolicy struct {
	Resources          ResourcePolicy      `json:"resources"`
	Containers         ContainerPolicy     `json:"containers"`
	ContainerOverrides []ContainerOverride `json:"container_overrides,omitempty"`
}

func defaultAlertPolicy() AlertPolicy {
	t := Thresholds{Warning: 80, Critical: 90}
	return AlertPolicy{Resources: ResourcePolicy{CPU: t, Memory: t, Disk: DiskPolicy{Thresholds: t, Forecast: DiskForecast{Days: 7, WindowHours: 72, Severity: "warning"}}},
		Containers: ContainerPolicy{Stopped: "warning", Unhealthy: "warning", Starting: "warning", RestartLoop: RestartLoop{Restarts: 3, WindowSeconds: 600, Severity: "warning"}}}
}

func (p AlertPolicy) Validate() error {
	for _, t := range []Thresholds{p.Resources.CPU, p.Resources.Memory, p.Resources.Disk.Thresholds} {
		if math.IsNaN(t.Warning) || math.IsNaN(t.Critical) || math.IsInf(t.Warning, 0) || math.IsInf(t.Critical, 0) || t.Warning < 0 || t.Warning >= t.Critical || t.Critical > 100 {
			return errors.New("alert_policy thresholds require 0 <= warning < critical <= 100")
		}
	}
	validSeverity := func(s string) bool { return s == "warning" || s == "critical" }
	if f := p.Resources.Disk.Forecast; math.IsNaN(f.Days) || f.Days < 0 || f.Days > 365 || f.WindowHours < 24 || f.WindowHours > 8760 || !validSeverity(f.Severity) {
		return errors.New("alert_policy disk forecast requires days 0-365, window_hours 24-8760 and warning/critical severity")
	}
	if !validSeverity(p.Containers.Stopped) || !validSeverity(p.Containers.Unhealthy) || !validSeverity(p.Containers.Starting) {
		return errors.New("alert_policy container severity must be warning or critical")
	}
	validLoop := func(r RestartLoop, override bool) bool {
		window := r.WindowSeconds >= 60 && r.WindowSeconds <= 86400
		if override && r.WindowSeconds == 0 {
			window = true
		}
		return r.Restarts >= 0 && r.Restarts <= 10000 && window && (validSeverity(r.Severity) || (override && r.Severity == ""))
	}
	if !validLoop(p.Containers.RestartLoop, false) {
		return errors.New("alert_policy restart_loop requires restarts >= 0, window_seconds 60-86400 and warning/critical severity")
	}
	if len(p.ContainerOverrides) > 2048 {
		return errors.New("alert_policy has too many container overrides")
	}
	seen := map[string]bool{}
	for _, rule := range p.ContainerOverrides {
		key := rule.Host + "\x00" + rule.Name
		lifecycle := rule.Lifecycle == "oneshot" || (rule.Lifecycle == "" && rule.Failure == "")
		if !ValidID(rule.Host) || rule.Name == "" || len(rule.Name) > 256 || strings.ContainsAny(rule.Name, "\x00\r\n\t /") || !lifecycle || (rule.Lifecycle == "" && rule.RestartLoop == nil) || (rule.Failure != "" && !validSeverity(rule.Failure)) || (rule.RestartLoop != nil && !validLoop(*rule.RestartLoop, true)) || seen[key] {
			return errors.New("alert_policy overrides require a unique host/name with a oneshot lifecycle (optional warning/critical failure) and/or a valid restart_loop")
		}
		seen[key] = true
	}
	return nil
}
