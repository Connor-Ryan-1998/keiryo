use std::{
    fs,
    io::{BufRead, BufReader},
    os::unix::fs::PermissionsExt,
    process::{Command, Stdio},
    sync::mpsc,
    time::Duration,
};

#[test]
fn startup_json_errors_report_location_without_config_values() {
    let dir = tempfile::tempdir().unwrap();
    let config = dir.path().join("agent.json");
    let secret = "private-credential-that-must-not-be-logged";
    fs::write(
        &config,
        format!("{{\n\"interval_seconds\":\"{secret}\"\n}}"),
    )
    .unwrap();
    let output = Command::new(env!("CARGO_BIN_EXE_monitoring-agent"))
        .args(["agent", "-config"])
        .arg(config)
        .output()
        .unwrap();
    let stderr = String::from_utf8(output.stderr).unwrap();
    assert!(!output.status.success());
    assert!(
        stderr.contains("line 2") && stderr.contains("column"),
        "missing JSON location: {stderr}"
    );
    assert!(!stderr.contains(secret), "configuration value exposed");
}

#[test]
fn queue_failure_logs_cause_without_credentials_or_payload() {
    let dir = tempfile::tempdir().unwrap();
    let token = "private-credential-that-must-not-be-logged";
    fs::write(dir.path().join("token"), token).unwrap();
    fs::set_permissions(dir.path().join("token"), fs::Permissions::from_mode(0o600)).unwrap();
    let config = dir.path().join("agent.json");
    fs::write(
        &config,
        serde_json::to_vec(&serde_json::json!({
            "host_id":"node", "hub_url":"http://127.0.0.1:1",
            "credential_file":"token", "queue_max_bytes":1,
            "docker_socket":"missing.sock", "interval_seconds":1
        }))
        .unwrap(),
    )
    .unwrap();
    let mut child = Command::new(env!("CARGO_BIN_EXE_monitoring-agent"))
        .args(["agent", "-config"])
        .arg(config)
        .stderr(Stdio::piped())
        .stdout(Stdio::null())
        .spawn()
        .unwrap();
    let stderr = child.stderr.take().unwrap();
    let (tx, rx) = mpsc::channel();
    let reader = std::thread::spawn(move || {
        for line in BufReader::new(stderr).lines() {
            let line = line.unwrap();
            if let Ok(event) = serde_json::from_str::<serde_json::Value>(&line)
                && event["event"] == "metrics_persistence_failed"
            {
                let _ = tx.send(event);
                break;
            }
        }
    });
    let event = rx.recv_timeout(Duration::from_secs(5));
    child.kill().unwrap();
    child.wait().unwrap();
    reader.join().unwrap();
    let event = event.expect("missing persistence failure event");
    assert!(
        event["error"]
            .as_str()
            .is_some_and(|s| s.contains("oversized")),
        "missing storage cause: {event}"
    );
    assert!(!event.to_string().contains(token));
    assert!(event.get("payload").is_none());
}
