use monitoring_agent::config::Config;
use std::{fs, os::unix::fs::PermissionsExt};
use tempfile::TempDir;

fn setup(extra: serde_json::Value) -> (TempDir, std::path::PathBuf) {
    let dir = TempDir::new().unwrap();
    fs::write(dir.path().join("node.token"), "a".repeat(32)).unwrap();
    fs::set_permissions(
        dir.path().join("node.token"),
        fs::Permissions::from_mode(0o600),
    )
    .unwrap();
    let mut json = serde_json::json!({"host_id":"node", "hub_url":"http://127.0.0.1:9081", "credential_file":"node.token"});
    for (k, v) in extra.as_object().unwrap() {
        json[k] = v.clone();
    }
    let path = dir.path().join("agent.json");
    fs::write(&path, serde_json::to_vec(&json).unwrap()).unwrap();
    (dir, path)
}

#[test]
fn old_config_uses_durable_time_and_byte_limits() {
    let (dir, path) = setup(serde_json::json!({"buffer_samples":1}));
    let c = Config::load(&path).unwrap();
    assert_eq!(c.queue_database, dir.path().join("agent.redb"));
    assert_eq!(c.queue_retention_seconds, 7200);
    assert_eq!(c.queue_max_bytes, 268435456);
    assert_eq!(c.credential_file, dir.path().join("node.token"));
}

#[test]
fn rejects_invalid_config_without_disclosing_credentials() {
    for extra in [
        serde_json::json!({"unknown":true}),
        serde_json::json!({"host_id":"../bad"}),
        serde_json::json!({"hub_url":"http://secret@example.com"}),
        serde_json::json!({"queue_max_bytes":0}),
        serde_json::json!({"queue_retention_seconds":0}),
        serde_json::json!({"otlp_metrics_url":"http://localhost/v1/metrics?secret=yes"}),
        serde_json::json!({"interval_seconds":0}),
        serde_json::json!({"buffer_samples":0}),
    ] {
        let (_dir, path) = setup(extra);
        let error = Config::load(&path).unwrap_err().to_string();
        assert!(!error.contains(&"a".repeat(32)));
    }
}

#[test]
fn refuses_shared_or_nonregular_token_file() {
    let (dir, path) = setup(serde_json::json!({}));
    fs::set_permissions(
        dir.path().join("node.token"),
        fs::Permissions::from_mode(0o644),
    )
    .unwrap();
    assert!(Config::load(&path).is_err());
    fs::remove_file(dir.path().join("node.token")).unwrap();
    fs::create_dir(dir.path().join("node.token")).unwrap();
    assert!(Config::load(&path).is_err());
}

#[test]
fn accepts_null_exclusion_lists_from_existing_go_json() {
    let (_dir, path) =
        setup(serde_json::json!({"exclude_interfaces":null,"exclude_filesystems":null}));
    let c = Config::load(&path).unwrap();
    assert!(c.exclude_interfaces.is_empty());
    assert!(c.exclude_filesystems.is_empty());
}

#[test]
fn configuration_errors_preserve_json_location_without_exposing_values() {
    let (_dir, path) = setup(serde_json::json!({}));
    let secret = "private-token-value-that-must-not-be-logged";
    fs::write(&path, format!("{{\n\"host_id\":\"{secret}\",\n")).unwrap();
    let error = Config::load(&path).unwrap_err();
    let cause = error
        .downcast_ref::<serde_json::Error>()
        .expect("JSON cause");
    assert!(cause.line() >= 2, "JSON location lost: {error:#}");
    assert!(!format!("{error:#}").contains(secret));
}

#[test]
fn missing_credential_preserves_io_error_kind() {
    let (dir, path) = setup(serde_json::json!({}));
    fs::remove_file(dir.path().join("node.token")).unwrap();
    let error = Config::load(&path).unwrap_err();
    assert_eq!(
        error
            .downcast_ref::<std::io::Error>()
            .expect("I/O cause")
            .kind(),
        std::io::ErrorKind::NotFound
    );
}
