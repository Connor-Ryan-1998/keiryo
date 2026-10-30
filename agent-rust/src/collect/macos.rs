use crate::{collect::Collection, config::Config};
use sysinfo::{Disks, Networks, System};

#[allow(deprecated)] // libc exposes these stable Mach APIs; keep the FFI boundary local.
pub fn collect(c: &Config, s: &mut Collection) {
    let boot = available(System::boot_time(), s, "boot time collection failed")
        .map(|seconds| (seconds as i64).saturating_mul(1_000_000_000))
        .unwrap_or(0);
    if let Some(uptime) = available(System::uptime(), s, "uptime collection failed") {
        s.add("system.uptime", "s", uptime as f64, &[], false, 0);
    }
    if let Some(load) = available(System::load_average(), s, "load average collection failed") {
        s.add(
            "system.cpu.load_average.1m",
            "{thread}",
            load.one,
            &[],
            false,
            0,
        );
    }
    if let Some(mut system) = available(System::new(), s, "memory collection failed") {
        system.refresh_memory();
        s.add(
            "system.memory.limit",
            "By",
            system.total_memory() as f64,
            &[],
            false,
            0,
        );
        s.add(
            "keiryo.memory.available",
            "By",
            system.available_memory() as f64,
            &[],
            false,
            0,
        );
        for (state, value) in [("free", system.free_swap()), ("used", system.used_swap())] {
            s.add(
                "system.paging.usage",
                "By",
                value as f64,
                &[("state", state)],
                false,
                0,
            );
        }
    }
    let mut ticks = [0i32; 4];
    let mut count = 4u32;
    // SAFETY: mach_host_self takes no arguments and returns an owned host send right.
    let host = unsafe { libc::mach_host_self() };
    // SAFETY: host is live; ticks holds the four integer_t values advertised by count.
    let result = unsafe {
        libc::host_statistics(
            host,
            libc::HOST_CPU_LOAD_INFO,
            ticks.as_mut_ptr(),
            &mut count,
        )
    };
    unsafe extern "C" {
        fn mach_port_deallocate(task: libc::mach_port_t, name: libc::mach_port_t) -> i32;
    }
    // SAFETY: host is the owned send right above, unused after this release in our task.
    unsafe { mach_port_deallocate(libc::mach_task_self(), host) };
    if result == 0 && count == 4 {
        for (state, index) in [
            ("user", Some(0)),
            ("system", Some(1)),
            ("idle", Some(2)),
            ("nice", Some(3)),
            ("wait", None),
            ("interrupt", None),
            ("softirq", None),
            ("steal", None),
        ] {
            s.add(
                "system.cpu.time",
                "s",
                index.map(|i| ticks[i] as u32 as f64 / 100.0).unwrap_or(0.0),
                &[("cpu", "all"), ("state", state)],
                true,
                boot,
            );
        }
    } else {
        s.state.errors.push("CPU collection failed".into());
    }
    if let Some(disks) = available(
        Disks::new_with_refreshed_list(),
        s,
        "filesystem collection failed",
    ) {
        for disk in &disks {
            let mount = disk.mount_point().to_string_lossy();
            let kind = disk.file_system().to_string_lossy();
            if c.exclude_filesystems
                .iter()
                .any(|v| v == mount.as_ref() || v == kind.as_ref())
                || matches!(kind.as_ref(), "devfs" | "autofs")
            {
                continue;
            }
            let total = disk.total_space();
            let free = disk.available_space();
            if free > total {
                continue;
            }
            for (state, value) in [("free", free), ("used", total - free), ("reserved", 0)] {
                s.add(
                    "system.filesystem.usage",
                    "By",
                    value as f64,
                    &[("mountpoint", &mount), ("state", state)],
                    false,
                    0,
                );
            }
        }
    }
    match super::macos_io::collect() {
        Ok(devices) => {
            for (name, read, write) in devices {
                for (direction, value) in [("read", read), ("write", write)] {
                    s.add(
                        "system.disk.io",
                        "By",
                        value as f64,
                        &[("device", &name), ("direction", direction)],
                        true,
                        boot,
                    );
                }
            }
        }
        Err(_) => s.state.errors.push("disk I/O collection failed".into()),
    }
    if let Some(networks) = available(
        Networks::new_with_refreshed_list(),
        s,
        "network collection failed",
    ) {
        for (name, net) in &networks {
            let name = name.to_string_lossy();
            if c.exclude_interfaces
                .iter()
                .any(|excluded| excluded == name.as_ref())
            {
                continue;
            }
            for (direction, value) in [
                ("receive", net.total_received()),
                ("transmit", net.total_transmitted()),
            ] {
                s.add(
                    "system.network.io",
                    "By",
                    value as f64,
                    &[("device", &name), ("direction", direction)],
                    true,
                    boot,
                );
            }
        }
    }
}

fn available<T>(
    result: Result<T, sysinfo::Error>,
    sample: &mut Collection,
    message: &str,
) -> Option<T> {
    match result {
        Ok(value) => Some(value),
        Err(_) => {
            sample.state.errors.push(message.into());
            None
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unavailable_sysinfo_values_report_errors_without_dropping_other_metrics() {
        let mut sample = Collection::new(&Config::default());
        sample.add("existing", "1", 1.0, &[], false, 0);
        assert!(
            available::<u64>(
                Err(sysinfo::Error::Unsupported),
                &mut sample,
                "memory collection failed"
            )
            .is_none()
        );
        assert_eq!(
            available(Ok(42), &mut sample, "uptime collection failed"),
            Some(42)
        );
        assert_eq!(sample.state.errors, ["memory collection failed"]);
        assert_eq!(sample.observations.len(), 1);
    }
}
