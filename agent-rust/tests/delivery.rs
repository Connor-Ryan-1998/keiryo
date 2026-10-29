use monitoring_agent::{
    config::Config,
    delivery::{self, SendOutcome},
};
use std::{
    fs,
    io::{Read, Write},
    net::TcpListener,
    os::unix::fs::PermissionsExt,
    time::Duration,
};
fn request(status: u16, content: &str, body: &[u8], extra: &str) -> SendOutcome {
    let dir = tempfile::tempdir().unwrap();
    let token = dir.path().join("token");
    fs::write(&token, "a".repeat(32)).unwrap();
    fs::set_permissions(&token, fs::Permissions::from_mode(0o600)).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let body = body.to_vec();
    let content = content.to_string();
    let extra = extra.to_string();
    let worker = std::thread::spawn(move || {
        let (mut conn, _) = listener.accept().unwrap();
        conn.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
        let mut data = [0u8; 8192];
        let n = conn.read(&mut data).unwrap();
        let request = String::from_utf8_lossy(&data[..n]).to_lowercase();
        assert!(request.contains("authorization: bearer "));
        assert!(request.contains("x-host-id: node"));
        write!(
            conn,
            "HTTP/1.1 {status} OK\r\nContent-Type: {content}\r\nContent-Length: {}\r\n{extra}\r\n",
            body.len()
        )
        .unwrap();
        conn.write_all(&body).unwrap();
    });
    let c = Config {
        host_id: "node".into(),
        hub_url: url,
        credential_file: token,
        ..Default::default()
    };
    let result = delivery::send(&delivery::client().unwrap(), &c, b"test");
    worker.join().unwrap();
    result
}
#[test]
fn success_is_validated_and_partial_or_permanent_is_not_retried() {
    assert_eq!(
        request(200, "application/x-protobuf", b"", ""),
        SendOutcome::Accepted
    );
    assert_eq!(
        request(
            200,
            "application/json",
            br#"{"partialSuccess":{"rejectedDataPoints":"2"}}"#,
            ""
        ),
        SendOutcome::Partial { rejected: 2 }
    );
    assert_eq!(
        request(400, "application/x-protobuf", b"", ""),
        SendOutcome::Permanent { status: 400 }
    );
    assert_eq!(
        request(302, "text/plain", b"", "Location: http://different/\r\n"),
        SendOutcome::Permanent { status: 302 }
    );
    assert_eq!(
        request(503, "application/x-protobuf", b"", "Retry-After: 10\r\n"),
        SendOutcome::Retry {
            after: Some(Duration::from_secs(10))
        }
    );
    assert_eq!(
        request(200, "application/x-protobuf", b"invalid", ""),
        SendOutcome::Retry { after: None }
    );
    assert_eq!(
        request(200, "text/html", b"", ""),
        SendOutcome::Retry { after: None }
    );
}
