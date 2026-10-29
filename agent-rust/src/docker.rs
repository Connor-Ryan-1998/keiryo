use crate::{
    Result,
    collect::{Collection, Container, Observation},
    config::{Config, valid_id},
};
use anyhow::{Context, ensure};
use reqwest::blocking::{Client, Response};
use serde_json::Value;
use std::{
    collections::BTreeMap,
    io::Read,
    time::{Duration, Instant},
};

pub struct Docker {
    client: Client,
    version: String,
    previous: BTreeMap<String, (u64, u64)>,
}
impl Docker {
    pub fn new(c: &Config) -> Result<Self> {
        Ok(Self {
            client: Client::builder()
                .unix_socket(c.docker_socket.as_path())
                .no_proxy()
                .timeout(Duration::from_secs(3))
                .redirect(reqwest::redirect::Policy::none())
                .build()?,
            version: String::new(),
            previous: BTreeMap::new(),
        })
    }
    fn get(&self, path: &str, timeout: Duration) -> Result<Response> {
        let res = self
            .client
            .get(format!("http://docker{path}"))
            .timeout(timeout)
            .send()
            .map_err(|_| anyhow::anyhow!("Docker unavailable"))?;
        ensure!(res.status().as_u16() == 200, "Docker read failed");
        Ok(res)
    }
    fn json_before(&self, path: &str, deadline: Instant) -> Result<Value> {
        self.json_timeout(path, remaining(deadline)?.min(Duration::from_secs(3)))
    }
    fn json_timeout(&self, path: &str, timeout: Duration) -> Result<Value> {
        let mut bytes = Vec::new();
        self.get(path, timeout)?
            .take(8 * 1024 * 1024 + 1)
            .read_to_end(&mut bytes)?;
        ensure!(bytes.len() <= 8 * 1024 * 1024, "oversized Docker response");
        Ok(serde_json::from_slice(&bytes)?)
    }
    fn api_before(&mut self, deadline: Instant) -> Result<String> {
        if self.version.is_empty() {
            let v = self.json_before("/version", deadline)?;
            let v = v["ApiVersion"].as_str().context("invalid Docker version")?;
            let minor = v.strip_prefix("1.").context("invalid Docker version")?;
            ensure!(
                !minor.is_empty() && minor.len() <= 3 && minor.bytes().all(|b| b.is_ascii_digit()),
                "invalid Docker version"
            );
            self.version = format!("/v{v}");
        }
        Ok(self.version.clone())
    }
    pub fn collect(&mut self, c: &Config, s: &mut Collection) -> Result<()> {
        let deadline = Instant::now() + Duration::from_secs(c.interval_seconds.min(50));
        let api = self.api_before(deadline)?;
        let list = self.json_before(&format!("{api}/containers/json?all=true"), deadline)?;
        let list = list.as_array().context("invalid Docker list")?;
        let mut next = BTreeMap::new();
        s.state.log_fetch = true;
        if list.len() > 512 {
            s.state
                .errors
                .push("Docker collection truncated at 512 containers".into());
        }
        for item in list.iter().take(512) {
            if Instant::now() >= deadline {
                s.state
                    .errors
                    .push("Docker collection deadline exceeded".into());
                break;
            }
            let Some(id) = item["Id"].as_str().filter(|s| valid_id(s)) else {
                continue;
            };
            let name = item["Names"][0]
                .as_str()
                .unwrap_or(id)
                .trim_start_matches('/');
            let mut out = Container {
                id: id.into(),
                name: name.chars().take(256).collect(),
                state: item["State"].as_str().unwrap_or("unknown").into(),
                ..Default::default()
            };
            match self.json_before(&format!("{api}/containers/{id}/json"), deadline) {
                Ok(v) => {
                    out.restarts = v["RestartCount"].as_u64().unwrap_or(0);
                    out.state = v["State"]["Status"].as_str().unwrap_or(&out.state).into();
                    out.health = v["State"]["Health"]["Status"].as_str().unwrap_or("").into();
                    if out.state == "exited" {
                        out.exit_code = v["State"]["ExitCode"]
                            .as_u64()
                            .and_then(|v| u8::try_from(v).ok());
                    }
                }
                Err(_) => s.state.errors.push("container inspection failed".into()),
            }
            let running = out.state == "running";
            s.state.containers.push(out);
            if !running {
                continue;
            }
            let Ok(v) = self.json_before(
                &format!("{api}/containers/{id}/stats?stream=false&one-shot=true"),
                deadline,
            ) else {
                s.state.errors.push("container stats unavailable".into());
                continue;
            };
            if let (Some(total), Some(system), Some(cpus)) = (
                v["cpu_stats"]["cpu_usage"]["total_usage"].as_u64(),
                v["cpu_stats"]["system_cpu_usage"].as_u64(),
                v["cpu_stats"]["online_cpus"].as_u64(),
            ) {
                if let Some((old_total, old_system)) = self.previous.get(id)
                    && total >= *old_total
                    && system > *old_system
                    && cpus > 0
                {
                    let value = (total - old_total) as f64 / (system - old_system) as f64
                        * cpus as f64
                        * 100.0;
                    s.observations.push(Observation {
                        name: "container.cpu.utilization".into(),
                        unit: "1".into(),
                        entity: id.into(),
                        value,
                        ..Default::default()
                    });
                }
                next.insert(id.into(), (total, system));
            }
            if let (Some(usage), Some(limit)) = (
                v["memory_stats"]["usage"].as_u64(),
                v["memory_stats"]["limit"].as_u64(),
            ) {
                let inactive = v["memory_stats"]["stats"]["inactive_file"]
                    .as_u64()
                    .unwrap_or(0);
                let memory = if inactive <= usage {
                    usage - inactive
                } else {
                    usage
                };
                for (name, value) in [
                    ("container.memory.usage.total", memory),
                    ("container.memory.usage.limit", limit),
                ] {
                    s.observations.push(Observation {
                        name: name.into(),
                        unit: "By".into(),
                        entity: id.into(),
                        value: value as f64,
                        ..Default::default()
                    });
                }
            }
            if s.state.errors.len() > 60 {
                s.state.errors.truncate(60);
            }
        }
        self.previous = next;
        Ok(())
    }
    pub fn logs(
        &mut self,
        id: &str,
        lines: usize,
        bytes: usize,
        timeout: Duration,
    ) -> Result<String> {
        ensure!(
            valid_id(id) && lines > 0 && bytes > 0,
            "invalid log request"
        );
        let deadline = Instant::now() + timeout;
        let api = self.api_before(deadline)?;
        let inspect = self.json_before(&format!("{api}/containers/{id}/json"), deadline)?;
        let tty = inspect["Config"]["Tty"].as_bool().unwrap_or(false);
        let res = self.get(
            &format!(
                "{api}/containers/{id}/logs?stdout=true&stderr=true&timestamps=true&tail={lines}"
            ),
            remaining(deadline)?,
        )?;
        decode_logs(res, tty, lines, bytes)
    }
}

fn remaining(deadline: Instant) -> Result<Duration> {
    let remaining = deadline.saturating_duration_since(Instant::now());
    ensure!(!remaining.is_zero(), "Docker request deadline exceeded");
    Ok(remaining)
}

pub fn decode_logs(reader: impl Read, tty: bool, lines: usize, limit: usize) -> Result<String> {
    ensure!(lines > 0 && limit > 0, "invalid log limits");
    let mut reader = reader.take((limit + 65536) as u64);
    let mut out = Vec::new();
    if tty {
        reader.take(limit as u64).read_to_end(&mut out)?;
    } else {
        while out.len() < limit {
            let mut header = [0u8; 8];
            let n = reader.read(&mut header[..1])?;
            if n == 0 {
                break;
            }
            reader.read_exact(&mut header[1..])?;
            ensure!(
                header[0] <= 2 && header[1..4] == [0, 0, 0],
                "invalid Docker log frame"
            );
            let length = u32::from_be_bytes(header[4..].try_into()?) as usize;
            let take = length.min(limit - out.len());
            let old = out.len();
            out.resize(old + take, 0);
            reader.read_exact(&mut out[old..])?;
            if take < length {
                break;
            }
        }
    }
    let text = String::from_utf8_lossy(&out);
    let mut bounded = text.len().min(limit);
    while !text.is_char_boundary(bounded) {
        bounded -= 1;
    }
    let text = &text[..bounded];
    let end = text
        .match_indices('\n')
        .nth(lines - 1)
        .map(|(i, _)| i + 1)
        .unwrap_or(text.len());
    Ok(text[..end].into())
}
