use monitoring_agent::{
    config::Config,
    delivery::{self, SendOutcome},
    outbox::*,
};
use std::{
    fs,
    io::{Read, Write},
    net::TcpListener,
    os::unix::fs::PermissionsExt,
    time::Duration,
};
#[test]
fn one_hour_of_committed_metrics_survives_restart_and_drains_without_reencoding() {
    let d = tempfile::tempdir().unwrap();
    let token = d.path().join("token");
    fs::write(&token, "a".repeat(32)).unwrap();
    fs::set_permissions(&token, fs::Permissions::from_mode(0o600)).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let c = Config {
        host_id: "node".into(),
        hub_url: url,
        credential_file: token,
        ..Default::default()
    };
    let identity = QueueIdentity {
        host_id: c.host_id.clone(),
        metrics_url: c.metrics_url(),
    };
    let limits = Limits {
        retention: Duration::from_secs(7200),
        max_bytes: 268435456,
    };
    let path = d.path().join("q.redb");
    let start = 1_000_000_000i64;
    let q = Outbox::open(&path, &identity, limits).unwrap();
    for i in 0..60 {
        q.enqueue(
            start + i * 60_000_000_000,
            format!("batch-{i}").as_bytes(),
            start + i * 60_000_000_000,
        )
        .unwrap();
    }
    drop(q);
    let q = Outbox::open(&path, &identity, limits).unwrap();
    let now = start + 3_600_000_000_000;
    assert_eq!(q.stats(now).unwrap().depth, 60);
    let server = std::thread::spawn(move || {
        for i in 0..60 {
            let (mut conn, _) = listener.accept().unwrap();
            let mut data = Vec::new();
            let mut buffer = [0u8; 1024];
            loop {
                let n = conn.read(&mut buffer).unwrap();
                assert!(n > 0);
                data.extend_from_slice(&buffer[..n]);
                if let Some(split) = data.windows(4).position(|v| v == b"\r\n\r\n") {
                    let head = String::from_utf8_lossy(&data[..split]).to_lowercase();
                    let length: usize = head
                        .lines()
                        .find_map(|v| v.strip_prefix("content-length: "))
                        .unwrap()
                        .trim()
                        .parse()
                        .unwrap();
                    if data.len() >= split + 4 + length {
                        assert_eq!(&data[split + 4..], format!("batch-{i}").as_bytes());
                        break;
                    }
                }
            }
            conn.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: application/x-protobuf\r\nContent-Length: 0\r\n\r\n").unwrap();
        }
    });
    let client = delivery::client().unwrap();
    for _ in 0..60 {
        assert_eq!(
            delivery::deliver_one(&client, &c, &q, now).unwrap(),
            Some(SendOutcome::Accepted)
        );
    }
    server.join().unwrap();
    assert_eq!(q.stats(now).unwrap().depth, 0);
}
