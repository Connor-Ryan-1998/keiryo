package config

import (
	"errors"
	"strings"
)

// ExpectedContainer declares a container that must exist on a host. Absence in a
// fresh container snapshot is a condition; stale snapshots prove nothing.
type ExpectedContainer struct {
	Host     string `json:"host"`
	Name     string `json:"name"`
	Severity string `json:"severity,omitempty"`
}

// ValidateExpectedContainers checks identities and applies the critical default.
func (c *Hub) ValidateExpectedContainers() error {
	if len(c.ExpectedContainers) > 2048 {
		return errors.New("too many expected containers")
	}
	seen := map[string]bool{}
	for i := range c.ExpectedContainers {
		e := &c.ExpectedContainers[i]
		if e.Severity == "" {
			e.Severity = "critical"
		}
		key := e.Host + "\x00" + e.Name
		_, configured := c.Agents[e.Host]
		if !ValidID(e.Host) || !configured || e.Name == "" || len(e.Name) > 256 || strings.ContainsAny(e.Name, "\x00\r\n\t /") || (e.Severity != "warning" && e.Severity != "critical") || seen[key] {
			return errors.New("expected_containers requires unique configured host/name pairs and warning/critical severity")
		}
		seen[key] = true
	}
	return nil
}
