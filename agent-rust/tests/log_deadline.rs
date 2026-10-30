use monitoring_agent::{config::Config, docker::Docker};
use std::{
    io::{Read, Write},
    os::unix::net::UnixListener,
    time::{Duration, Instant},
};
#[test]
fn log_deadline_includes_version_negotiation_and_inspection() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("d.sock");
    let listener = UnixListener::bind(&path).unwrap();
    listener.set_nonblocking(true).unwrap();
    let server = std::thread::spawn(move || {
        let deadline = Instant::now() + Duration::from_secs(1);
        while Instant::now() < deadline {
            let Ok((mut conn, _)) = listener.accept() else {
                std::thread::sleep(Duration::from_millis(1));
                continue;
            };
            conn.set_nonblocking(false).unwrap();
            conn.set_read_timeout(Some(Duration::from_secs(1))).unwrap();
            let mut buffer = [0u8; 4096];
            let n = conn.read(&mut buffer).unwrap();
            let request = String::from_utf8_lossy(&buffer[..n]);
            let body = if request.contains("GET /version ") {
                r#"{"ApiVersion":"1.47"}"#
            } else if request.contains("/logs?") {
                "line\n"
            } else {
                r#"{"Config":{"Tty":true}}"#
            };
            std::thread::sleep(Duration::from_millis(80));
            let _ = write!(
                conn,
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{}",
                body.len(),
                body
            );
        }
    });
    let c = Config {
        docker_socket: path,
        ..Default::default()
    };
    let mut docker = Docker::new(&c).unwrap();
    let start = Instant::now();
    let result = docker.logs("abc", 10, 100, Duration::from_millis(150));
    let elapsed = start.elapsed();
    assert!(
        elapsed < Duration::from_millis(220),
        "log deadline ignored preparation: {elapsed:?}"
    );
    assert!(result.is_err());
    server.join().unwrap();
}
