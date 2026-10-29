use crate::{Result, config::Config};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

#[derive(Clone, Default, Serialize, Deserialize)]
pub struct Observation {
    pub name: String,
    pub unit: String,
    pub value: f64,
    #[serde(default)]
    pub entity: String,
    #[serde(default)]
    pub attributes: BTreeMap<String, String>,
    #[serde(default)]
    pub start_ns: i64,
    #[serde(default)]
    pub counter: bool,
}
#[derive(Clone, Default, Serialize, Deserialize)]
pub struct Container {
    pub id: String,
    pub name: String,
    pub state: String,
    #[serde(skip_serializing_if = "String::is_empty", default)]
    pub health: String,
    pub restarts: u64,
    #[serde(skip_serializing_if = "Option::is_none", default)]
    pub exit_code: Option<u8>,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct AgentState {
    pub version: u32,
    pub host_id: String,
    pub id: String,
    pub at: DateTime<Utc>,
    pub display_name: String,
    pub agent_version: String,
    pub containers: Vec<Container>,
    pub errors: Vec<String>,
    pub log_fetch: bool,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Collection {
    pub at_ns: i64,
    pub observations: Vec<Observation>,
    pub state: AgentState,
}
impl Collection {
    pub fn new(c: &Config) -> Self {
        let at = Utc::now();
        Self {
            at_ns: at.timestamp_nanos_opt().unwrap(),
            observations: vec![],
            state: AgentState {
                version: 1,
                host_id: c.host_id.clone(),
                id: uuid::Uuid::new_v4().simple().to_string(),
                at,
                display_name: c.display_name.clone(),
                agent_version: env!("CARGO_PKG_VERSION").into(),
                containers: vec![],
                errors: vec![],
                log_fetch: false,
            },
        }
    }
    pub fn add(
        &mut self,
        name: &str,
        unit: &str,
        value: f64,
        attrs: &[(&str, &str)],
        counter: bool,
        start_ns: i64,
    ) {
        self.observations.push(Observation {
            name: name.into(),
            unit: unit.into(),
            value,
            attributes: attrs
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect(),
            counter,
            start_ns,
            ..Default::default()
        });
    }
}
pub mod linux;
#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "macos")]
mod macos_io;
pub struct Collector {
    docker: Option<crate::docker::Docker>,
}
impl Collector {
    pub fn new() -> Self {
        Self { docker: None }
    }
    pub fn collect(&mut self, c: &Config) -> Result<Collection> {
        let mut s = Collection::new(c);
        #[cfg(target_os = "linux")]
        linux::collect(c, &mut s);
        #[cfg(target_os = "macos")]
        macos::collect(c, &mut s);
        if self.docker.is_none() {
            self.docker = crate::docker::Docker::new(c).ok();
        }
        if let Some(docker) = &mut self.docker {
            if docker.collect(c, &mut s).is_err() {
                s.state.errors.push("Docker collection failed".into());
            }
        } else {
            s.state.errors.push("Docker unavailable".into());
        }
        s.state.errors.truncate(64);
        Ok(s)
    }
}
impl Default for Collector {
    fn default() -> Self {
        Self::new()
    }
}
