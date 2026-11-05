use crate::{Result, collect::Collection, config::Config};
use anyhow::ensure;
use serde::Deserialize;
use std::{fs::File, io::Read, os::unix::fs::OpenOptionsExt, path::Path};

#[derive(Deserialize)]
#[serde(rename_all = "snake_case")]
enum Outcome {
    Running,
    Success,
    Failure,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Status {
    version: u32,
    started_at: u64,
    finished_at: u64,
    last_success_at: u64,
    result: Outcome,
}

fn read_status(path: &Path, now: u64) -> Result<Status> {
    // Never block on a FIFO/device or follow a substituted status-file symlink.
    let file = File::options()
        .read(true)
        .custom_flags(libc::O_NONBLOCK | libc::O_NOFOLLOW)
        .open(path)?;
    ensure!(
        file.metadata()?.is_file(),
        "backup status must be a regular file"
    );
    let mut bytes = Vec::new();
    file.take(4097).read_to_end(&mut bytes)?;
    ensure!(bytes.len() <= 4096, "backup status exceeds limit");
    let status: Status = serde_json::from_slice(&bytes)?;
    ensure!(
        status.version == 1
            && status.started_at > 0
            && status.started_at <= now.saturating_add(300)
            && status.finished_at <= now.saturating_add(300)
            && status.last_success_at <= now.saturating_add(300),
        "invalid backup status timestamps"
    );
    match status.result {
        Outcome::Running => ensure!(
            status.finished_at == 0 && status.last_success_at <= status.started_at,
            "invalid running backup"
        ),
        Outcome::Success => ensure!(
            status.finished_at >= status.started_at && status.last_success_at == status.finished_at,
            "invalid successful backup"
        ),
        Outcome::Failure => ensure!(
            status.finished_at >= status.started_at && status.last_success_at <= status.started_at,
            "invalid failed backup"
        ),
    }
    Ok(status)
}

pub fn collect(c: &Config, sample: &mut Collection) {
    for (job, file) in &c.backup_status_files {
        let attributes = [("backup.job", job.as_str())];
        match read_status(file, (sample.at_ns / 1_000_000_000).max(0) as u64) {
            Ok(status) => {
                let result = match status.result {
                    Outcome::Running => 0.0,
                    Outcome::Success => 1.0,
                    Outcome::Failure => 2.0,
                };
                for (name, unit, value) in [
                    ("keiryo.backup.status_valid", "1", 1.0),
                    ("keiryo.backup.result", "1", result),
                    ("keiryo.backup.started", "s", status.started_at as f64),
                    ("keiryo.backup.finished", "s", status.finished_at as f64),
                    (
                        "keiryo.backup.last_success",
                        "s",
                        status.last_success_at as f64,
                    ),
                ] {
                    sample.add(name, unit, value, &attributes, false, 0);
                }
            }
            Err(_) => sample.add(
                "keiryo.backup.status_valid",
                "1",
                0.0,
                &attributes,
                false,
                0,
            ),
        }
    }
}
