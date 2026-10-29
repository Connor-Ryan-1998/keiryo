use monitoring_agent::{collect::Collector, config::Config};
#[test]
fn host_collection_survives_missing_docker_and_emits_complete_cpu_states() {
    let c = Config {
        host_id: "node".into(),
        docker_socket: "/nonexistent/docker.sock".into(),
        ..Default::default()
    };
    let s = Collector::new().collect(&c).unwrap();
    assert_eq!(
        s.observations
            .iter()
            .filter(|m| m.name == "system.cpu.time")
            .count(),
        8
    );
    assert!(
        s.observations
            .iter()
            .any(|m| m.name == "system.memory.limit" && m.value > 0.0)
    );
    assert!(!s.state.log_fetch);
    assert!(!s.state.errors.is_empty());
}

#[cfg(target_os = "macos")]
#[test]
fn macos_disk_io_is_device_based_and_independent_of_filesystem_exclusions() {
    let c = Config {
        host_id: "node".into(),
        exclude_filesystems: vec!["apfs".into()],
        docker_socket: "/nonexistent/docker.sock".into(),
        ..Default::default()
    };
    let s = Collector::new().collect(&c).unwrap();
    let disks: Vec<_> = s
        .observations
        .iter()
        .filter(|m| m.name == "system.disk.io")
        .collect();
    assert!(
        !disks.is_empty(),
        "excluding filesystems must not disable device I/O"
    );
    assert!(
        disks
            .iter()
            .all(|m| m.attributes["device"].starts_with("disk"))
    );
    assert!(disks.iter().any(|m| m.value > 0.0));
}
