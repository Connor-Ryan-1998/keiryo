use crate::{
    Result,
    collect::AgentState,
    config::{Config, read_secret, valid_id},
    delivery,
    docker::Docker,
};
use chrono::{DateTime, Utc};
use serde::Deserialize;
use std::{
    io::Read,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
pub type Latest = Arc<Mutex<Option<AgentState>>>;

pub fn wait(stop: &AtomicBool, duration: Duration) {
    let end = std::time::Instant::now() + duration;
    while !stop.load(Ordering::Relaxed) {
        let left = end.saturating_duration_since(std::time::Instant::now());
        if left.is_zero() {
            break;
        }
        std::thread::sleep(left.min(Duration::from_millis(100)));
    }
}

pub fn run(c: Config, latest: Latest, stop: Arc<AtomicBool>) -> Result<()> {
    let client = delivery::client()?;
    let mut sent = String::new();
    while !stop.load(Ordering::Relaxed) {
        let state = latest
            .lock()
            .map_err(|_| anyhow::anyhow!("state worker failed"))?
            .clone();
        if let Some(state) = state
            && state.id != sent
            && let Ok(token) = read_secret(&c.credential_file)
        {
            let response = client
                .post(format!("{}/v1/state", c.hub_url))
                .bearer_auth(token)
                .header("X-Host-ID", &c.host_id)
                .json(&state)
                .send();
            if response.is_ok_and(|v| v.status().as_u16() == 204) {
                sent = state.id;
            }
        }
        wait(&stop, Duration::from_secs(1));
    }
    Ok(())
}

#[derive(Deserialize)]
struct LogRequest {
    id: String,
    host_id: String,
    container_id: String,
    lines: usize,
    bytes: usize,
    expires: DateTime<Utc>,
}

pub fn logs(c: Config, latest: Latest, stop: Arc<AtomicBool>) -> Result<()> {
    let client = delivery::client()?;
    let mut docker = Docker::new(&c)?;
    while !stop.load(Ordering::Relaxed) {
        wait(&stop, Duration::from_secs(c.poll_seconds));
        if stop.load(Ordering::Relaxed) {
            break;
        }
        if !latest
            .lock()
            .map_err(|_| anyhow::anyhow!("state worker failed"))?
            .as_ref()
            .is_some_and(|v| v.log_fetch)
        {
            continue;
        }
        let Ok(token) = read_secret(&c.credential_file) else {
            continue;
        };
        let Ok(response) = client
            .get(format!("{}/v1/logs/next", c.hub_url))
            .bearer_auth(&token)
            .header("X-Host-ID", &c.host_id)
            .send()
        else {
            continue;
        };
        if response.status().as_u16() != 200 {
            continue;
        }
        let mut data = Vec::new();
        if response.take(8193).read_to_end(&mut data).is_err() || data.len() > 8192 {
            continue;
        }
        let Ok(req) = serde_json::from_slice::<LogRequest>(&data) else {
            continue;
        };
        if req.host_id != c.host_id || !valid_id(&req.id) || !valid_id(&req.container_id) {
            continue;
        }
        let remaining = (req.expires - Utc::now())
            .to_std()
            .unwrap_or_default()
            .min(Duration::from_secs(c.log_timeout_seconds));
        if remaining.is_zero() {
            continue;
        }
        let output = match docker.logs(
            &req.container_id,
            req.lines.clamp(1, c.log_lines),
            req.bytes.clamp(1, c.log_bytes),
            remaining,
        ) {
            Ok(text) => serde_json::json!({"id":req.id,"text":text}),
            Err(_) => {
                serde_json::json!({"id":req.id,"text":"","error":"container logs unavailable"})
            }
        };
        let _ = client
            .post(format!("{}/v1/logs/result", c.hub_url))
            .bearer_auth(token)
            .header("X-Host-ID", &c.host_id)
            .json(&output)
            .send();
    }
    Ok(())
}
