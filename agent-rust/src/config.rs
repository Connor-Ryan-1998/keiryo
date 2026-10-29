use crate::Result;
use anyhow::{Context, bail, ensure};
use serde::Deserialize;
use std::{
    fs::File,
    io::Read,
    os::unix::fs::{OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
};
use url::Url;

#[derive(Clone, Debug, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Config {
    pub host_id: String,
    pub display_name: String,
    pub hub_url: String,
    pub otlp_metrics_url: Option<String>,
    pub credential_file: PathBuf,
    pub interval_seconds: u64,
    pub buffer_samples: usize,
    pub poll_seconds: u64,
    pub docker_socket: PathBuf,
    #[serde(deserialize_with = "null_vec")]
    pub exclude_interfaces: Vec<String>,
    #[serde(deserialize_with = "null_vec")]
    pub exclude_filesystems: Vec<String>,
    pub log_bytes: usize,
    pub log_lines: usize,
    pub log_timeout_seconds: u64,
    pub queue_database: PathBuf,
    pub queue_retention_seconds: u64,
    pub queue_max_bytes: u64,
}

fn null_vec<'de, D: serde::Deserializer<'de>>(d: D) -> std::result::Result<Vec<String>, D::Error> {
    Ok(Option::<Vec<String>>::deserialize(d)?.unwrap_or_default())
}

impl Default for Config {
    fn default() -> Self {
        Self {
            host_id: String::new(),
            display_name: String::new(),
            hub_url: String::new(),
            otlp_metrics_url: None,
            credential_file: PathBuf::new(),
            interval_seconds: 60,
            buffer_samples: 60,
            poll_seconds: 5,
            docker_socket: "/var/run/docker.sock".into(),
            exclude_interfaces: vec!["lo".into()],
            exclude_filesystems: vec![],
            log_bytes: 262144,
            log_lines: 200,
            log_timeout_seconds: 15,
            queue_database: "agent.redb".into(),
            queue_retention_seconds: 7200,
            queue_max_bytes: 268435456,
        }
    }
}

pub fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id.as_bytes()[0].is_ascii_alphanumeric()
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
}

pub fn read_secret(path: &Path) -> Result<String> {
    let f = File::options()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
        .context("cannot open credential file")?;
    let metadata = f.metadata()?;
    ensure!(
        metadata.is_file() && metadata.permissions().mode() & 0o077 == 0,
        "credential file must be regular and mode 0600 or stricter"
    );
    let mut bytes = Vec::new();
    f.take(4097).read_to_end(&mut bytes)?;
    ensure!(bytes.len() <= 4096, "invalid credential file");
    let token =
        String::from_utf8(bytes).map_err(|_| anyhow::anyhow!("invalid credential encoding"))?;
    let token = token.trim();
    ensure!(
        token.len() >= 32
            && !token
                .bytes()
                .any(|b| b.is_ascii_whitespace() || b.is_ascii_control()),
        "invalid credential format"
    );
    Ok(token.into())
}

impl Config {
    pub fn load(path: &Path) -> Result<Self> {
        let mut bytes = Vec::new();
        File::open(path)?.take(1048577).read_to_end(&mut bytes)?;
        ensure!(bytes.len() <= 1048576, "configuration too large");
        let mut c: Self = serde_json::from_slice(&bytes)
            .context("invalid JSON configuration or unknown field")?;
        ensure!(
            valid_id(&c.host_id)
                && c.display_name.len() <= 256
                && !c.credential_file.as_os_str().is_empty(),
            "invalid host identity or credential path"
        );
        let hub = checked_url(&c.hub_url)?;
        ensure!(hub.path() == "/", "hub_url must be an HTTP(S) origin");
        c.hub_url = c.hub_url.trim_end_matches('/').into();
        checked_url(&c.metrics_url())?;
        ensure!(
            (1..=86400).contains(&c.interval_seconds)
                && (1..=1440).contains(&c.buffer_samples)
                && (1..=60).contains(&c.poll_seconds)
                && (1..=4194304).contains(&c.log_bytes)
                && (1..=10000).contains(&c.log_lines)
                && (1..=60).contains(&c.log_timeout_seconds),
            "invalid agent resource limits"
        );
        ensure!(
            c.queue_retention_seconds > 0
                && c.queue_retention_seconds <= 31536000
                && c.queue_max_bytes > 0
                && c.queue_max_bytes <= i64::MAX as u64
                && !c.queue_database.as_os_str().is_empty()
                && !c.docker_socket.as_os_str().is_empty(),
            "invalid queue or Docker configuration"
        );
        let parent = std::path::absolute(path)?.parent().unwrap().to_path_buf();
        for p in [
            &mut c.credential_file,
            &mut c.docker_socket,
            &mut c.queue_database,
        ] {
            if p.is_relative() {
                *p = parent.join(&*p);
            }
        }
        read_secret(&c.credential_file)?;
        Ok(c)
    }

    pub fn metrics_url(&self) -> String {
        self.otlp_metrics_url
            .clone()
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| format!("{}/v1/metrics", self.hub_url.trim_end_matches('/')))
    }
}

fn checked_url(raw: &str) -> Result<Url> {
    let Ok(u) = Url::parse(raw) else {
        bail!("endpoint requires a full HTTP(S) URL");
    };
    ensure!(
        matches!(u.scheme(), "http" | "https")
            && u.host_str().is_some()
            && u.username().is_empty()
            && u.password().is_none()
            && u.query().is_none()
            && u.fragment().is_none(),
        "endpoint requires HTTP(S) without credentials or query/fragment"
    );
    Ok(u)
}
