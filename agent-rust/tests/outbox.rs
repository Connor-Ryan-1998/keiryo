use monitoring_agent::outbox::*;
use redb::{ReadableDatabase, ReadableTable};
use std::{fs, os::unix::fs::PermissionsExt, time::Duration};
const T: i64 = 1_000_000_000;
fn identity() -> QueueIdentity {
    QueueIdentity {
        host_id: "node".into(),
        metrics_url: "http://127.0.0.1/v1/metrics".into(),
    }
}
fn limits() -> Limits {
    Limits {
        retention: Duration::from_secs(7200),
        max_bytes: 10,
    }
}

#[test]
fn fifo_bytes_accounting_and_sequence_survive_reopen() {
    let d = tempfile::tempdir().unwrap();
    let p = d.path().join("queue.redb");
    let q = Outbox::open(&p, &identity(), limits()).unwrap();
    q.enqueue(T, b"one", T).unwrap();
    let queued = q.enqueue(2 * T, b"two", 20 * T).unwrap();
    assert_eq!(queued.oldest_age_seconds, 19);
    assert!(Outbox::open(&p, &identity(), limits()).is_err());
    drop(q);
    let q = Outbox::open(&p, &identity(), limits()).unwrap();
    let b = q.oldest(2 * T).unwrap().unwrap();
    assert_eq!(b.payload, b"one");
    assert_eq!(b.observed_at_ns, T);
    q.ack(b.sequence).unwrap();
    q.ack(b.sequence).unwrap();
    let stats = q.stats(2 * T).unwrap();
    assert_eq!((stats.depth, stats.payload_bytes), (1, 3));
    assert_eq!(q.stats(20 * T).unwrap().oldest_age_seconds, 18);
    let b = q.oldest(2 * T).unwrap().unwrap();
    q.ack(b.sequence).unwrap();
    let previous = b.sequence;
    drop(q);
    let q = Outbox::open(&p, &identity(), limits()).unwrap();
    q.enqueue(3 * T, b"new", 3 * T).unwrap();
    assert!(q.oldest(3 * T).unwrap().unwrap().sequence > previous);
}

#[test]
fn expiry_eviction_and_oversize_are_explicit() {
    let d = tempfile::tempdir().unwrap();
    let q = Outbox::open(&d.path().join("q.redb"), &identity(), limits()).unwrap();
    q.enqueue(T, b"123456", T).unwrap();
    assert!(q.enqueue(2 * T, b"12345678901", 2 * T).is_err());
    assert_eq!(q.oldest(2 * T).unwrap().unwrap().payload, b"123456");
    q.enqueue(2 * T, b"abcdef", 2 * T).unwrap();
    let s = q.stats(2 * T).unwrap();
    assert_eq!((s.depth, s.evicted), (1, 1));
    assert!(q.oldest(7201 * T).unwrap().is_some());
    assert!(q.oldest(7202 * T).unwrap().is_none());
    assert_eq!(q.stats(3 * T).unwrap().expired, 1);
    assert!(q.oldest(3 * T).unwrap().is_none());
}

#[test]
fn identity_changes_require_empty_queue_and_corruption_is_preserved() {
    let d = tempfile::tempdir().unwrap();
    let p = d.path().join("q.redb");
    let q = Outbox::open(&p, &identity(), limits()).unwrap();
    q.enqueue(T, b"one", T).unwrap();
    drop(q);
    let mut other = identity();
    other.host_id = "other".into();
    assert!(Outbox::open(&p, &other, limits()).is_err());
    other = identity();
    other.metrics_url = "http://different/v1/metrics".into();
    assert!(Outbox::open(&p, &other, limits()).is_err());
    let q = Outbox::open(&p, &identity(), limits()).unwrap();
    let b = q.oldest(T).unwrap().unwrap();
    q.ack(b.sequence).unwrap();
    drop(q);
    drop(Outbox::open(&p, &other, limits()).unwrap());
    let bad = d.path().join("bad.redb");
    fs::write(&bad, b"not a database").unwrap();
    fs::set_permissions(&bad, fs::Permissions::from_mode(0o600)).unwrap();
    assert!(Outbox::open(&bad, &identity(), limits()).is_err());
    assert_eq!(fs::read(&bad).unwrap(), b"not a database");
}

#[test]
fn unsupported_schema_is_not_reinitialized() {
    let d = tempfile::tempdir().unwrap();
    let path = d.path().join("q.redb");
    drop(Outbox::open(&path, &identity(), limits()).unwrap());
    let db = redb::Database::open(&path).unwrap();
    let tx = db.begin_write().unwrap();
    {
        let mut table = tx
            .open_table(redb::TableDefinition::<&str, &[u8]>::new("metadata"))
            .unwrap();
        let mut meta: serde_json::Value =
            serde_json::from_slice(table.get("state").unwrap().unwrap().value()).unwrap();
        meta["schema"] = serde_json::json!(999);
        table
            .insert("state", serde_json::to_vec(&meta).unwrap().as_slice())
            .unwrap();
    }
    tx.commit().unwrap();
    drop(db);
    assert!(Outbox::open(&path, &identity(), limits()).is_err());
    let db = redb::Database::open(&path).unwrap();
    let tx = db.begin_read().unwrap();
    let table = tx
        .open_table(redb::TableDefinition::<&str, &[u8]>::new("metadata"))
        .unwrap();
    let meta: serde_json::Value =
        serde_json::from_slice(table.get("state").unwrap().unwrap().value()).unwrap();
    assert_eq!(meta["schema"], 999);
}

#[test]
fn queue_file_reuses_space_after_repeated_drain() {
    let d = tempfile::tempdir().unwrap();
    let path = d.path().join("q.redb");
    let q = Outbox::open(
        &path,
        &identity(),
        Limits {
            retention: std::time::Duration::from_secs(7200),
            max_bytes: 268435456,
        },
    )
    .unwrap();
    let payload = vec![42u8; 32768];
    let mut sizes = Vec::new();
    for _ in 0..3 {
        for i in 0..60 {
            q.enqueue(T + i * 60 * T, &payload, T + i * 60 * T).unwrap();
        }
        while let Some(batch) = q.oldest(3600 * T).unwrap() {
            q.ack(batch.sequence).unwrap();
        }
        sizes.push(fs::metadata(&path).unwrap().len());
        assert_eq!(q.stats(3600 * T).unwrap().depth, 0);
    }
    eprintln!("60 x 32KiB payload queue after drain, physical bytes per round: {sizes:?}");
    assert!(
        sizes[2] <= sizes[1] * 2,
        "unbounded growth under fixed-size churn"
    );
}
