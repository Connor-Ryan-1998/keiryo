package config

import "errors"

type BackupJob struct {
	Host          string `json:"host"`
	Job           string `json:"job"`
	MaxAgeSeconds int    `json:"max_age_seconds"`
}

func (c Hub) ValidateBackupJobs() error {
	if len(c.BackupJobs) > 2048 {
		return errors.New("too many expected backup jobs")
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, job := range c.BackupJobs {
		key := job.Host + "/" + job.Job
		_, configured := c.Agents[job.Host]
		counts[job.Host]++
		if !ValidID(job.Host) || !ValidID(job.Job) || !configured || seen[key] || counts[job.Host] > 64 || job.MaxAgeSeconds < 60 || job.MaxAgeSeconds > 31536000 {
			return errors.New("backup_jobs requires unique configured host/job identities and max_age_seconds between 60 and 31536000")
		}
		seen[key] = true
	}
	return nil
}
