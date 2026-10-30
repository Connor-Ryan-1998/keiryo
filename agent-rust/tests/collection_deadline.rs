use monitoring_agent::{collect::Collection, config::Config, docker::Docker};
use std::{
    io::{Read, Write},
    os::unix::net::UnixListener,
    time::{Duration, Instant},
};

#[test]
fn collection_deadline_covers_negotiation_list_inspection_and_stats() {
    let mut overshoots = Vec::new();
    for slow in [
        "/version",
        "/containers/json?",
        "/containers/abc/json",
        "/stats?",
        "cumulative",
    ] {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("docker.sock");
        let listener = UnixListener::bind(&socket).unwrap();
        listener.set_nonblocking(true).unwrap();
        let server = std::thread::spawn(move || {
            let deadline = Instant::now() + Duration::from_millis(1600);
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
                } else if request.contains("/containers/json?") {
                    r#"[{"Id":"abc","Names":["/app"],"State":"running"}]"#
                } else if request.contains("/stats?") {
                    r#"{"memory_stats":{"usage":40,"limit":100}}"#
                } else {
                    r#"{"RestartCount":0,"State":{"Status":"running"}}"#
                };
                if slow == "cumulative" {
                    std::thread::sleep(Duration::from_millis(400));
                } else if request.contains(slow) {
                    std::thread::sleep(Duration::from_millis(1400));
                }
                let _ = write!(
                    conn,
                    "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: {}\r\n\r\n{body}",
                    body.len()
                );
            }
        });
        let c = Config {
            interval_seconds: 1,
            docker_socket: socket,
            ..Default::default()
        };
        let mut docker = Docker::new(&c).unwrap();
        let mut collection = Collection::new(&c);
        let start = Instant::now();
        let result = docker.collect(&c, &mut collection);
        let elapsed = start.elapsed();
        server.join().unwrap();
        if elapsed >= Duration::from_millis(1250) {
            overshoots.push((slow, elapsed));
        }
        assert!(
            result.is_err() || !collection.state.errors.is_empty(),
            "{slow} timeout was not reported"
        );
        if slow == "/stats?" || slow == "cumulative" {
            assert_eq!(
                collection.state.containers.len(),
                1,
                "completed inspection lost"
            );
        }
    }
    assert!(
        overshoots.is_empty(),
        "collection budget exceeded: {overshoots:?}"
    );
}
