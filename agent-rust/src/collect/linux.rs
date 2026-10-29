use crate::{Result, collect::Collection, config::Config};
use anyhow::{Context, ensure};
use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
};

pub fn parse_cpu(stat: &str, ticks: f64, s: &mut Collection) -> Result<i64> {
    ensure!(ticks > 0.0, "invalid clock ticks");
    let boot = stat
        .lines()
        .find_map(|l| l.strip_prefix("btime "))
        .context("missing boot time")?
        .trim()
        .parse::<i64>()?
        .checked_mul(1_000_000_000)
        .context("invalid boot time")?;
    let line = stat
        .lines()
        .find(|l| l.starts_with("cpu "))
        .context("missing aggregate CPU")?;
    let values = line
        .split_whitespace()
        .skip(1)
        .map(str::parse::<u64>)
        .collect::<std::result::Result<Vec<_>, _>>()?;
    ensure!(values.len() >= 8, "incomplete CPU states");
    for (i, state) in [
        "user",
        "nice",
        "system",
        "idle",
        "wait",
        "interrupt",
        "softirq",
        "steal",
    ]
    .iter()
    .enumerate()
    {
        s.add(
            "system.cpu.time",
            "s",
            values[i] as f64 / ticks,
            &[("cpu", "all"), ("state", state)],
            true,
            boot,
        );
    }
    Ok(boot)
}

pub fn parse_memory(text: &str, s: &mut Collection) -> Result<()> {
    let mut values = BTreeMap::new();
    for line in text.lines() {
        if let Some((key, value)) = line.split_once(':') {
            let mut parts = value.split_whitespace();
            if let Some(n) = parts.next() {
                values.insert(
                    key,
                    n.parse::<u64>()?
                        .checked_mul(1024)
                        .context("memory overflow")?,
                );
            }
        }
    }
    let total = *values.get("MemTotal").context("missing memory total")?;
    let available = *values
        .get("MemAvailable")
        .context("missing available memory")?;
    ensure!(available <= total, "invalid available memory");
    s.add("system.memory.limit", "By", total as f64, &[], false, 0);
    s.add(
        "system.linux.memory.available",
        "By",
        available as f64,
        &[],
        false,
        0,
    );
    if let (Some(total), Some(free)) = (values.get("SwapTotal"), values.get("SwapFree")) {
        ensure!(free <= total, "invalid swap");
        s.add(
            "system.paging.usage",
            "By",
            *free as f64,
            &[("state", "free")],
            false,
            0,
        );
        s.add(
            "system.paging.usage",
            "By",
            (total - free) as f64,
            &[("state", "used")],
            false,
            0,
        );
    }
    Ok(())
}

pub fn parse_network(text: &str, c: &Config, boot: i64, s: &mut Collection) -> Result<()> {
    for line in text.lines() {
        let Some((name, data)) = line.rsplit_once(':') else {
            continue;
        };
        let name = name.trim();
        if c.exclude_interfaces.iter().any(|v| v == name) {
            continue;
        }
        let values = data
            .split_whitespace()
            .map(str::parse::<u64>)
            .collect::<std::result::Result<Vec<_>, _>>()?;
        ensure!(values.len() >= 16, "invalid network counters");
        for (direction, value) in [("receive", values[0]), ("transmit", values[8])] {
            s.add(
                "system.network.io",
                "By",
                value as f64,
                &[("device", name), ("direction", direction)],
                true,
                boot,
            );
        }
    }
    Ok(())
}

pub fn filesystems(proc: &Path, root: &Path, c: &Config, s: &mut Collection) -> Result<()> {
    let text = fs::read_to_string(proc.join("1/mounts"))?;
    for line in text.lines() {
        let parts: Vec<_> = line.split_whitespace().collect();
        ensure!(parts.len() >= 3, "invalid mount record");
        let mount = parts[1]
            .replace("\\040", " ")
            .replace("\\011", "\t")
            .replace("\\134", "\\");
        let kind = parts[2];
        if matches!(
            kind,
            "proc"
                | "sysfs"
                | "devtmpfs"
                | "devpts"
                | "tmpfs"
                | "cgroup"
                | "cgroup2"
                | "securityfs"
                | "debugfs"
                | "tracefs"
                | "pstore"
                | "mqueue"
                | "hugetlbfs"
                | "configfs"
                | "fusectl"
                | "autofs"
                | "rpc_pipefs"
        ) || c
            .exclude_filesystems
            .iter()
            .any(|v| v == &mount || v == kind)
        {
            continue;
        }
        if !mount.starts_with('/') {
            continue;
        }
        let path = root.join(mount.trim_start_matches('/'));
        let path = std::ffi::CString::new(path.as_os_str().as_encoded_bytes())?;
        let mut stat = std::mem::MaybeUninit::<libc::statvfs>::uninit();
        // SAFETY: path is NUL-terminated and stat points to writable, aligned storage.
        if unsafe { libc::statvfs(path.as_ptr(), stat.as_mut_ptr()) } != 0 {
            continue;
        }
        // SAFETY: statvfs returned success and initialized stat.
        let stat = unsafe { stat.assume_init() };
        let size = stat.f_frsize as f64;
        let total = stat.f_blocks as f64 * size;
        let free = stat.f_bavail as f64 * size;
        if free > total {
            continue;
        }
        for (state, value) in [("free", free), ("used", total - free), ("reserved", 0.0)] {
            s.add(
                "system.filesystem.usage",
                "By",
                value,
                &[("mountpoint", &mount), ("state", state)],
                false,
                0,
            );
        }
    }
    Ok(())
}

pub fn collect(c: &Config, s: &mut Collection) {
    let proc = std::env::var_os("HOST_PROC")
        .map(PathBuf::from)
        .unwrap_or_else(|| "/proc".into());
    let mut boot = 0;
    let cpu = (|| -> Result<()> {
        let stat = fs::read_to_string(proc.join("stat"))?;
        // SAFETY: _SC_CLK_TCK is a valid sysconf selector and requires no pointers.
        boot = parse_cpu(&stat, unsafe { libc::sysconf(libc::_SC_CLK_TCK) } as f64, s)?;
        Ok(())
    })();
    if cpu.is_err() {
        s.state.errors.push("CPU collection failed".into());
    }
    if fs::read_to_string(proc.join("meminfo"))
        .map_err(Into::into)
        .and_then(|v| parse_memory(&v, s))
        .is_err()
    {
        s.state.errors.push("memory collection failed".into());
    }
    for (file, name) in [
        ("uptime", "system.uptime"),
        ("loadavg", "system.cpu.load_average.1m"),
    ] {
        let value = fs::read_to_string(proc.join(file))
            .ok()
            .and_then(|v| v.split_whitespace().next()?.parse::<f64>().ok());
        if let Some(value) = value {
            s.add(
                name,
                if file == "uptime" { "s" } else { "{thread}" },
                value,
                &[],
                false,
                0,
            );
        } else {
            s.state.errors.push(format!("{file} collection failed"));
        }
    }
    let net = if std::env::var_os("HOST_ROOT").is_some() {
        proc.join("1/net/dev")
    } else {
        proc.join("net/dev")
    };
    if fs::read_to_string(net)
        .map_err(Into::into)
        .and_then(|v| parse_network(&v, c, boot, s))
        .is_err()
    {
        s.state.errors.push("network collection failed".into());
    }
    if let Ok(text) = fs::read_to_string(proc.join("diskstats")) {
        for line in text.lines() {
            let values: Vec<_> = line.split_whitespace().collect();
            if values.len() < 14 {
                continue;
            }
            let name = values[2];
            if name.starts_with("loop") || name.starts_with("ram") {
                continue;
            }
            for (direction, index) in [("read", 5), ("write", 9)] {
                if let Ok(sectors) = values[index].parse::<u64>() {
                    s.add(
                        "system.disk.io",
                        "By",
                        sectors as f64 * 512.0,
                        &[("device", name), ("direction", direction)],
                        true,
                        boot,
                    );
                }
            }
        }
    } else {
        s.state.errors.push("disk I/O collection failed".into());
    }
    let root = std::env::var_os("HOST_ROOT")
        .map(PathBuf::from)
        .unwrap_or_else(|| "/".into());
    if filesystems(&proc, &root, c, s).is_err() {
        s.state.errors.push("filesystem collection failed".into());
    }
}
