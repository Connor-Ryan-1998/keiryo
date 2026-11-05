package hub

import (
	"fmt"
	"houselab/monitoring/internal/store"
	"time"
)

func (h *Hub) backupSignals(host store.Host) []condition {
	values := metrics(host)
	var out []condition
	for _, job := range h.config.BackupJobs {
		if job.Host != host.ID {
			continue
		}
		c := condition{Host: host.ID, Name: hostName(host), Key: "backup/" + job.Job, Kind: "Backup", State: "unavailable", Detail: job.Job + ": status missing, invalid or stale"}
		valid, v := values["backup_status_valid\x00"+job.Job]
		result, r := values["backup_result\x00"+job.Job]
		last, l := values["backup_last_success\x00"+job.Job]
		if v && valid == 1 && r && l {
			c.State = "healthy"
			c.Detail = job.Job + ": last successful backup " + time.Unix(int64(last), 0).UTC().Format(time.RFC3339)
			if last <= 0 || time.Now().Unix()-int64(last) > int64(job.MaxAgeSeconds) {
				c.State = "critical"
				c.Detail = fmt.Sprintf("%s: no successful backup within %d seconds", job.Job, job.MaxAgeSeconds)
			}
			if result == 2 {
				c.State = "critical"
				c.Detail = job.Job + ": latest backup failed"
			}
		}
		out = append(out, c)
	}
	return out
}
