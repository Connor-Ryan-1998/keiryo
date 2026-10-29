use monitoring_agent::{
    collect::{
        Collection,
        linux::{parse_cpu, parse_memory, parse_network},
    },
    config::Config,
};
#[test]
fn proc_fixtures_preserve_units_available_memory_cpu_states_and_exclusions() {
    let c = Config {
        host_id: "node".into(),
        ..Default::default()
    };
    let mut s = Collection::new(&c);
    assert_eq!(
        parse_cpu("cpu 100 20 30 400 50 6 7 8 0 0\nbtime 100\n", 100.0, &mut s).unwrap(),
        100_000_000_000
    );
    assert_eq!(s.observations.len(), 8);
    assert_eq!(
        s.observations
            .iter()
            .find(|m| m.attributes.get("state").map(String::as_str) == Some("idle"))
            .unwrap()
            .value,
        4.0
    );
    parse_memory(
        "MemTotal: 100 kB\nMemAvailable: 60 kB\nMemFree: 20 kB\nSwapTotal: 10 kB\nSwapFree: 8 kB\n",
        &mut s,
    )
    .unwrap();
    assert_eq!(
        s.observations
            .iter()
            .find(|m| m.name == "system.linux.memory.available")
            .unwrap()
            .value,
        61440.0
    );
    parse_network(
        "lo: 1 0 0 0 0 0 0 0 2 0 0 0 0 0 0 0\neth0: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0",
        &c,
        100,
        &mut s,
    )
    .unwrap();
    let net: Vec<_> = s
        .observations
        .iter()
        .filter(|m| m.name == "system.network.io")
        .collect();
    assert_eq!(net.len(), 2);
    assert_eq!(net[0].value, 100.0);
    assert_eq!(net[1].value, 200.0);
    assert!(parse_cpu("cpu 1 2\nbtime 100", 100.0, &mut s).is_err());
    assert!(parse_memory("MemTotal: 100 kB\nMemFree: 20 kB", &mut s).is_err());
}

#[test]
fn filesystems_use_pid_one_mounts_and_prefix_host_root_once() {
    let dir = tempfile::tempdir().unwrap();
    let proc = dir.path().join("proc");
    let root = dir.path().join("host");
    std::fs::create_dir_all(proc.join("1")).unwrap();
    std::fs::create_dir_all(root.join("data")).unwrap();
    std::fs::write(
        proc.join("mounts"),
        "wrong /host/container-only ext4 rw 0 0\n",
    )
    .unwrap();
    std::fs::write(
        proc.join("1/mounts"),
        "disk / ext4 rw 0 0\ndisk /data ext4 rw 0 0\n",
    )
    .unwrap();
    let c = Config::default();
    let mut s = Collection::new(&c);
    monitoring_agent::collect::linux::filesystems(&proc, &root, &c, &mut s).unwrap();
    let mounts: std::collections::BTreeSet<_> = s
        .observations
        .iter()
        .filter_map(|m| m.attributes.get("mountpoint").cloned())
        .collect();
    assert_eq!(
        mounts,
        std::collections::BTreeSet::from(["/".into(), "/data".into()])
    );
}
