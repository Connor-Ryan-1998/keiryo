use monitoring_agent::{
    collect::{Collection, backups},
    config::Config,
    otlp,
};
use opentelemetry_proto::tonic::collector::metrics::v1::ExportMetricsServiceRequest;
use prost::Message;
use std::{fs, os::unix::fs::symlink};

#[test]
fn backup_status_is_bounded_typed_and_exported_over_otlp() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("status.json");
    let c = Config {
        host_id: "node".into(),
        backup_status_files: [("daily".into(), path.clone())].into(),
        ..Default::default()
    };
    for (status, expected) in [
        (
            r#"{"version":1,"started_at":90,"finished_at":100,"last_success_at":100,"result":"success"}"#,
            Some(1.0),
        ),
        (
            r#"{"version":1,"started_at":90,"finished_at":100,"last_success_at":80,"result":"failure"}"#,
            Some(2.0),
        ),
        (
            r#"{"version":1,"started_at":90,"finished_at":0,"last_success_at":80,"result":"running"}"#,
            Some(0.0),
        ),
        (
            r#"{"version":1,"started_at":90,"finished_at":100,"last_success_at":80,"result":"success"}"#,
            None,
        ),
        (
            r#"{"version":1,"started_at":90,"finished_at":80,"last_success_at":80,"result":"failure"}"#,
            None,
        ),
        (
            r#"{"version":1,"started_at":9999,"finished_at":0,"last_success_at":0,"result":"running"}"#,
            None,
        ),
        (r#"{"result":"success"}"#, None),
        (
            r#"{"version":2,"started_at":90,"finished_at":100,"last_success_at":100,"result":"success"}"#,
            None,
        ),
        ("not json", None),
    ] {
        fs::write(&path, status).unwrap();
        let mut s = Collection::new(&c);
        s.at_ns = 101_000_000_000;
        backups::collect(&c, &mut s);
        assert_eq!(
            s.observations
                .iter()
                .find(|o| o.name == "keiryo.backup.result")
                .map(|o| o.value),
            expected
        );
        let valid = s
            .observations
            .iter()
            .find(|o| o.name == "keiryo.backup.status_valid")
            .unwrap();
        assert_eq!(valid.value, if expected.is_some() { 1.0 } else { 0.0 });
        let request =
            ExportMetricsServiceRequest::decode(otlp::encode(&s, &c).unwrap().as_slice()).unwrap();
        assert_eq!(request.resource_metrics.len(), 1);
        assert!(
            s.observations
                .iter()
                .all(|o| o.attributes.get("backup.job").unwrap() == "daily")
        );
        assert!(
            s.state.errors.is_empty(),
            "backup status failures use job metrics, not unbounded error labels"
        );
    }
    for content in [vec![b' '; 4097], vec![]] {
        fs::write(&path, content).unwrap();
        let mut s = Collection::new(&c);
        backups::collect(&c, &mut s);
        assert_eq!(s.observations.len(), 1);
        assert_eq!(s.observations[0].value, 0.0);
    }
    fs::remove_file(&path).unwrap();
    symlink("/dev/zero", &path).unwrap();
    let mut s = Collection::new(&c);
    backups::collect(&c, &mut s);
    assert_eq!(s.observations[0].value, 0.0);
    fs::remove_file(&path).unwrap();
    let mut s = Collection::new(&c);
    backups::collect(&c, &mut s);
    assert_eq!(s.observations[0].value, 0.0);
}
