use crate::{collect::Collection, config::Config};
use sysinfo::{Disks, Networks, System};

#[allow(deprecated)] // libc exposes these stable Mach APIs; keep the FFI boundary local.
pub fn collect(c: &Config, s: &mut Collection) {
    let mut system = System::new();
    system.refresh_memory();
    let boot = (System::boot_time() as i64).saturating_mul(1_000_000_000);
    s.add("system.uptime", "s", System::uptime() as f64, &[], false, 0);
    s.add(
        "system.cpu.load_average.1m",
        "{thread}",
        System::load_average().one,
        &[],
        false,
        0,
    );
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
    let disks = Disks::new_with_refreshed_list();
    for disk in disks.list() {
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
    for (name, net) in &Networks::new_with_refreshed_list() {
        if c.exclude_interfaces.contains(name) {
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
                &[("device", name), ("direction", direction)],
                true,
                boot,
            );
        }
    }
}
