package hub

import (
	"encoding/json"
	"fmt"
	"houselab/monitoring/internal/protocol"
)

func (h *Hub) containerState(host string, c protocol.Container) string {
	if c.State == "unknown" {
		return "unavailable"
	}
	p := h.config.AlertPolicy
	for _, rule := range p.ContainerOverrides {
		if rule.Host == host && rule.Name == c.Name && rule.Lifecycle == "oneshot" && c.State == "exited" && c.ExitCode != nil {
			if *c.ExitCode == 0 {
				return "healthy"
			}
			if rule.Failure != "" {
				return rule.Failure
			}
			return p.Containers.Stopped
		}
	}
	if c.State != "running" {
		return p.Containers.Stopped
	}
	if c.Health == "unhealthy" {
		return p.Containers.Unhealthy
	}
	if c.Health == "starting" {
		return p.Containers.Starting
	}
	return "healthy"
}

func containerDetail(c protocol.Container) string {
	detail := c.Name + ": " + c.State
	if c.Health != "" {
		detail += " / " + c.Health
	}
	if c.State == "exited" {
		if c.ExitCode == nil {
			detail += "; exit code unknown"
		} else {
			detail += fmt.Sprintf("; exit code %d", *c.ExitCode)
		}
	}
	return detail
}

type metricView struct {
	Value *float64
	State string
}

func (h *Hub) metricReading(resource string, v *float64) metricView {
	p := h.config.AlertPolicy.Resources
	state := "unavailable"
	switch resource {
	case "cpu":
		state = p.CPU.State(v)
	case "memory":
		state = p.Memory.State(v)
	case "disk":
		state = p.Disk.State(v)
	}
	return metricView{v, state}
}

func (h *Hub) resourcePolicyJSON() (string, error) {
	b, err := json.Marshal(h.config.AlertPolicy.Resources)
	return string(b), err
}
