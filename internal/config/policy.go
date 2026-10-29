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
	Disk   Thresholds `json:"disk"`
}

type ContainerPolicy struct {
	Stopped   string `json:"stopped"`
	Unhealthy string `json:"unhealthy"`
	Starting  string `json:"starting"`
}

type ContainerOverride struct {
	Host      string `json:"host"`
	Name      string `json:"name"`
	Lifecycle string `json:"lifecycle"`
	Failure   string `json:"failure,omitempty"`
}

type AlertPolicy struct {
	Resources          ResourcePolicy      `json:"resources"`
	Containers         ContainerPolicy     `json:"containers"`
	ContainerOverrides []ContainerOverride `json:"container_overrides,omitempty"`
}

func defaultAlertPolicy() AlertPolicy {
	t := Thresholds{Warning: 80, Critical: 90}
	return AlertPolicy{Resources: ResourcePolicy{CPU: t, Memory: t, Disk: t},
		Containers: ContainerPolicy{Stopped: "warning", Unhealthy: "warning", Starting: "warning"}}
}

func (p AlertPolicy) Validate() error {
	for _, t := range []Thresholds{p.Resources.CPU, p.Resources.Memory, p.Resources.Disk} {
		if math.IsNaN(t.Warning) || math.IsNaN(t.Critical) || math.IsInf(t.Warning, 0) || math.IsInf(t.Critical, 0) || t.Warning < 0 || t.Warning >= t.Critical || t.Critical > 100 {
			return errors.New("alert_policy thresholds require 0 <= warning < critical <= 100")
		}
	}
	validSeverity := func(s string) bool { return s == "warning" || s == "critical" }
	if !validSeverity(p.Containers.Stopped) || !validSeverity(p.Containers.Unhealthy) || !validSeverity(p.Containers.Starting) {
		return errors.New("alert_policy container severity must be warning or critical")
	}
	if len(p.ContainerOverrides) > 2048 {
		return errors.New("alert_policy has too many container overrides")
	}
	seen := map[string]bool{}
	for _, rule := range p.ContainerOverrides {
		key := rule.Host + "\x00" + rule.Name
		if !ValidID(rule.Host) || rule.Name == "" || len(rule.Name) > 256 || strings.ContainsAny(rule.Name, "\x00\r\n\t /") || rule.Lifecycle != "oneshot" || (rule.Failure != "" && !validSeverity(rule.Failure)) || seen[key] {
			return errors.New("alert_policy overrides require a unique host/name, oneshot lifecycle and optional warning/critical failure")
		}
		seen[key] = true
	}
	return nil
}
