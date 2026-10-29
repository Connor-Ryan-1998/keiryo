use crate::Result;
use anyhow::{Context, ensure};
use redb::{Database, Durability, ReadableTable, ReadableTableMetadata, TableDefinition};
use serde::{Deserialize, Serialize};
use std::{
    fs::{self, File, OpenOptions},
    os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt},
    path::Path,
    time::Duration,
};

const PENDING: TableDefinition<u64, &[u8]> = TableDefinition::new("pending");
const META: TableDefinition<&str, &[u8]> = TableDefinition::new("metadata");
#[cfg(test)]
thread_local! {static SYNCED_DIRECTORIES:std::cell::RefCell<Vec<std::path::PathBuf>>=const {std::cell::RefCell::new(Vec::new())};}

#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct QueueIdentity {
    pub host_id: String,
    pub metrics_url: String,
}
#[derive(Clone, Copy)]
pub struct Limits {
    pub retention: Duration,
    pub max_bytes: u64,
}
pub struct PendingBatch {
    pub sequence: u64,
    pub observed_at_ns: i64,
    pub payload: Vec<u8>,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct QueueStats {
    pub depth: u64,
    pub payload_bytes: u64,
    pub expired: u64,
    pub evicted: u64,
    pub permanent: u64,
    pub rejected_points: u64,
    pub oldest_age_seconds: u64,
}
#[derive(Serialize, Deserialize)]
struct Metadata {
    schema: u32,
    identity: QueueIdentity,
    next: u64,
    high_water: i64,
    stats: QueueStats,
}
pub struct Outbox {
    db: Database,
    limits: Limits,
    #[cfg(test)]
    fail_commit: std::sync::atomic::AtomicBool,
}

impl Outbox {
    pub fn open(path: &Path, identity: &QueueIdentity, limits: Limits) -> Result<Self> {
        ensure!(
            !limits.retention.is_zero() && limits.max_bytes > 0,
            "invalid queue limits"
        );
        let absolute = std::path::absolute(path)?;
        if let Some(parent) = absolute.parent() {
            fs::DirBuilder::new()
                .recursive(true)
                .mode(0o700)
                .create(parent)?;
        }
        let mut builder = Database::builder();
        builder.set_cache_size(8 * 1024 * 1024);
        let db = match OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(path)
        {
            Ok(file) => builder.create_file(file)?,
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
                let m = fs::symlink_metadata(path)?;
                ensure!(
                    m.is_file() && m.permissions().mode() & 0o077 == 0,
                    "queue database must be regular and mode 0600 or stricter"
                );
                builder
                    .open(path)
                    .context("cannot open durable queue; preserve file for recovery")?
            }
            Err(e) => return Err(e.into()),
        };
        let q = Self {
            db,
            limits,
            #[cfg(test)]
            fail_commit: std::sync::atomic::AtomicBool::new(false),
        };
        let mut tx = q.db.begin_write()?;
        tx.set_durability(Durability::Immediate)?;
        {
            let pending = tx.open_table(PENDING)?;
            let mut table = tx.open_table(META)?;
            let old = table.get("state")?.map(|v| v.value().to_vec());
            let meta = if let Some(bytes) = old {
                let mut m: Metadata = serde_json::from_slice(&bytes)?;
                ensure!(
                    m.schema == 1,
                    "unsupported durable queue schema; preserve database"
                );
                ensure!(
                    m.identity == *identity || pending.is_empty()?,
                    "queued metrics belong to another host or destination; restore old configuration and drain first"
                );
                m.identity = identity.clone();
                m
            } else {
                ensure!(
                    pending.is_empty()?,
                    "queue metadata missing; preserve database"
                );
                Metadata {
                    schema: 1,
                    identity: identity.clone(),
                    next: 1,
                    high_water: 0,
                    stats: QueueStats::default(),
                }
            };
            table.insert("state", serde_json::to_vec(&meta)?.as_slice())?;
        }
        tx.commit()?;
        // File fsync alone does not persist the new file or directory links.
        for directory in absolute
            .parent()
            .context("queue needs a parent directory")?
            .ancestors()
        {
            File::open(directory)?
                .sync_all()
                .context("cannot persist queue directory entry")?;
            #[cfg(test)]
            SYNCED_DIRECTORIES.with(|paths| paths.borrow_mut().push(directory.to_path_buf()));
        }
        Ok(q)
    }

    fn update<T>(
        &self,
        now: Option<i64>,
        change: impl FnOnce(&mut redb::Table<u64, &[u8]>, &mut Metadata) -> Result<T>,
    ) -> Result<T> {
        let mut tx = self.db.begin_write()?;
        tx.set_durability(Durability::Immediate)?;
        let value = {
            let mut records = tx.open_table(PENDING)?;
            let mut table = tx.open_table(META)?;
            let bytes = table
                .get("state")?
                .context("queue metadata missing")?
                .value()
                .to_vec();
            let mut m: Metadata = serde_json::from_slice(&bytes)?;
            if let Some(now) = now {
                m.high_water = m.high_water.max(now);
                let cutoff = m
                    .high_water
                    .saturating_sub(self.limits.retention.as_nanos().min(i64::MAX as u128) as i64);
                let mut expired = Vec::new();
                for item in records.iter()? {
                    let (key, value) = item?;
                    let data = value.value();
                    ensure!(data.len() >= 8, "invalid queue record");
                    if i64::from_le_bytes(data[..8].try_into()?) <= cutoff {
                        expired.push(key.value());
                    }
                }
                for key in expired {
                    remove(&mut records, &mut m, key)?;
                    m.stats.expired += 1;
                }
                while m.stats.payload_bytes > self.limits.max_bytes {
                    evict(&mut records, &mut m)?;
                }
            }
            let result = change(&mut records, &mut m)?;
            table.insert("state", serde_json::to_vec(&m)?.as_slice())?;
            result
        };
        #[cfg(test)]
        anyhow::ensure!(
            !self
                .fail_commit
                .swap(false, std::sync::atomic::Ordering::Relaxed),
            "injected storage commit failure"
        );
        tx.commit()?;
        Ok(value)
    }

    pub fn enqueue(&self, observed_at_ns: i64, payload: &[u8], now_ns: i64) -> Result<QueueStats> {
        ensure!(
            observed_at_ns > 0
                && !payload.is_empty()
                && payload.len() <= 2 * 1024 * 1024
                && payload.len() as u64 <= self.limits.max_bytes,
            "invalid or oversized metrics batch"
        );
        self.update(Some(now_ns), |records, m| {
            let age = m.high_water.saturating_sub(observed_at_ns);
            if age as u128 >= self.limits.retention.as_nanos() && age >= 0 {
                m.stats.expired += 1;
                return queue_stats(records, m);
            }
            while m.stats.payload_bytes > self.limits.max_bytes - payload.len() as u64 {
                evict(records, m)?;
            }
            let sequence = m.next;
            m.next = m.next.checked_add(1).context("queue sequence exhausted")?;
            let mut data = observed_at_ns.to_le_bytes().to_vec();
            data.extend_from_slice(payload);
            records.insert(sequence, data.as_slice())?;
            m.stats.depth += 1;
            m.stats.payload_bytes += payload.len() as u64;
            queue_stats(records, m)
        })
    }

    pub fn oldest(&self, now_ns: i64) -> Result<Option<PendingBatch>> {
        self.update(Some(now_ns), |records, _| {
            let Some((key, value)) = records.first()? else {
                return Ok(None);
            };
            let data = value.value();
            ensure!(data.len() >= 8, "invalid queue record");
            Ok(Some(PendingBatch {
                sequence: key.value(),
                observed_at_ns: i64::from_le_bytes(data[..8].try_into()?),
                payload: data[8..].to_vec(),
            }))
        })
    }

    pub fn ack(&self, sequence: u64) -> Result<()> {
        self.finish(sequence, false, 0)
    }
    pub fn finish(&self, sequence: u64, permanent: bool, rejected_points: u64) -> Result<()> {
        self.update(None, |records, m| {
            if remove(records, m, sequence)? {
                m.stats.permanent += u64::from(permanent);
                m.stats.rejected_points += rejected_points;
            }
            Ok(())
        })
    }
    pub fn stats(&self, now_ns: i64) -> Result<QueueStats> {
        self.update(Some(now_ns), |records, m| queue_stats(records, m))
    }
}

fn queue_stats(records: &redb::Table<u64, &[u8]>, m: &Metadata) -> Result<QueueStats> {
    let mut stats = m.stats.clone();
    if let Some((_, value)) = records.first()? {
        let data = value.value();
        ensure!(data.len() >= 8, "invalid queue record");
        stats.oldest_age_seconds = (m
            .high_water
            .saturating_sub(i64::from_le_bytes(data[..8].try_into()?))
            .max(0)
            / 1_000_000_000) as u64;
    }
    Ok(stats)
}

fn remove(records: &mut redb::Table<u64, &[u8]>, m: &mut Metadata, key: u64) -> Result<bool> {
    if let Some(value) = records.remove(key)? {
        ensure!(value.value().len() >= 8, "invalid queue record");
        m.stats.depth = m
            .stats
            .depth
            .checked_sub(1)
            .context("invalid queue accounting")?;
        m.stats.payload_bytes = m
            .stats
            .payload_bytes
            .checked_sub((value.value().len() - 8) as u64)
            .context("invalid queue byte accounting")?;
        Ok(true)
    } else {
        Ok(false)
    }
}
fn evict(records: &mut redb::Table<u64, &[u8]>, m: &mut Metadata) -> Result<()> {
    let key = records
        .first()?
        .context("invalid queue accounting")?
        .0
        .value();
    remove(records, m, key)?;
    m.stats.evicted += 1;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::Ordering;
    #[test]
    fn creation_syncs_new_directory_links_and_database_parent() {
        let dir = tempfile::tempdir().unwrap();
        let parent = dir.path().join("nested/queue");
        let path = parent.join("agent.redb");
        let q = Outbox::open(
            &path,
            &QueueIdentity {
                host_id: "node".into(),
                metrics_url: "http://localhost".into(),
            },
            Limits {
                retention: Duration::from_secs(7200),
                max_bytes: 100,
            },
        )
        .unwrap();
        q.enqueue(1, b"one", 1).unwrap();
        SYNCED_DIRECTORIES.with(|paths| {
            let paths = paths.borrow();
            for expected in [dir.path().to_path_buf(), dir.path().join("nested"), parent] {
                assert!(
                    paths.contains(&expected),
                    "directory link not synced: {expected:?}"
                );
            }
        });
    }
    #[test]
    fn commit_failure_rolls_back_enqueue_and_acknowledgement() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("q.redb");
        let identity = QueueIdentity {
            host_id: "node".into(),
            metrics_url: "http://localhost/v1/metrics".into(),
        };
        let q = Outbox::open(
            &path,
            &identity,
            Limits {
                retention: Duration::from_secs(7200),
                max_bytes: 100,
            },
        )
        .unwrap();
        q.fail_commit.store(true, Ordering::Relaxed);
        assert!(q.enqueue(1, b"one", 1).is_err());
        assert_eq!(q.stats(1).unwrap().depth, 0);
        q.enqueue(1, b"one", 1).unwrap();
        let batch = q.oldest(1).unwrap().unwrap();
        q.fail_commit.store(true, Ordering::Relaxed);
        assert!(q.ack(batch.sequence).is_err());
        assert_eq!(q.oldest(1).unwrap().unwrap().payload, b"one");
    }
}
