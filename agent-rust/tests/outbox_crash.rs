use monitoring_agent::outbox::*;
use std::{
    io::{BufRead, BufReader, Write},
    process::{Command, Stdio},
    time::Duration,
};
fn identity() -> QueueIdentity {
    QueueIdentity {
        host_id: "node".into(),
        metrics_url: "http://localhost/v1/metrics".into(),
    }
}
fn limits() -> Limits {
    Limits {
        retention: Duration::from_secs(7200),
        max_bytes: 268435456,
    }
}

#[test]
#[ignore]
fn crash_child() {
    let path = std::env::var("QUEUE_CRASH_PATH").unwrap();
    let mode = std::env::var("QUEUE_CRASH_MODE").unwrap();
    if mode == "uncommitted" {
        let db = redb::Database::open(&path).unwrap();
        let tx = db.begin_write().unwrap();
        {
            let mut table = tx
                .open_table(redb::TableDefinition::<u64, &[u8]>::new("pending"))
                .unwrap();
            table.insert(99, b"uncommitted".as_slice()).unwrap();
        }
        println!("READY");
        std::io::stdout().flush().unwrap();
        std::thread::park();
        drop(tx);
    } else {
        let q = Outbox::open(std::path::Path::new(&path), &identity(), limits()).unwrap();
        if mode == "enqueue" {
            q.enqueue(1_000_000_000, b"committed", 1_000_000_000)
                .unwrap();
        }
        if mode == "ack" {
            q.ack(1).unwrap();
        }
        println!("READY");
        std::io::stdout().flush().unwrap();
        std::thread::park();
    }
}

#[test]
fn kill_at_commit_boundaries_preserves_complete_transactions() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("queue.redb");
    for mode in ["enqueue", "uncommitted", "ack"] {
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "crash_child", "--ignored", "--nocapture"])
            .env("QUEUE_CRASH_PATH", &path)
            .env("QUEUE_CRASH_MODE", mode)
            .stdout(Stdio::piped())
            .spawn()
            .unwrap();
        let mut reader = BufReader::new(child.stdout.take().unwrap());
        let mut line = String::new();
        loop {
            line.clear();
            assert!(
                reader.read_line(&mut line).unwrap() > 0,
                "child failed before checkpoint"
            );
            if line.trim() == "READY" {
                break;
            }
        }
        child.kill().unwrap();
        child.wait().unwrap();
        let q = Outbox::open(&path, &identity(), limits()).unwrap();
        let s = q.stats(1_000_000_000).unwrap();
        if mode == "ack" {
            assert_eq!((s.depth, s.payload_bytes), (0, 0));
        } else {
            assert_eq!((s.depth, s.payload_bytes), (1, 9));
            assert_eq!(
                q.oldest(1_000_000_000).unwrap().unwrap().payload,
                b"committed"
            );
        }
    }
}
