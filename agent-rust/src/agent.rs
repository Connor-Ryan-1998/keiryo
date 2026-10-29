use crate::{
    Result,
    collect::Collector,
    config::Config,
    control::{self, Latest},
    delivery::{self, SendOutcome},
    otlp,
    outbox::{Limits, Outbox, QueueIdentity},
};
use anyhow::Context;
use chrono::Utc;
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};

#[derive(Default)]
struct Health {
    persistence_failures: AtomicU64,
    enqueue_failed: AtomicBool,
    delivery_failed: AtomicBool,
}

pub fn run(c: Config, stop: Arc<AtomicBool>) -> Result<()> {
    let q = Arc::new(Outbox::open(
        &c.queue_database,
        &QueueIdentity {
            host_id: c.host_id.clone(),
            metrics_url: c.metrics_url(),
        },
        Limits {
            retention: Duration::from_secs(c.queue_retention_seconds),
            max_bytes: c.queue_max_bytes,
        },
    )?);
    q.stats(now())?;
    let latest: Latest = Arc::new(Mutex::new(None));
    let health = Arc::new(Health::default());
    std::thread::scope(|scope| -> Result<()> {
        let state = scope.spawn({
            let c = c.clone();
            let latest = latest.clone();
            let stop = stop.clone();
            move || control::run(c, latest, stop)
        });
        let logs = scope.spawn({
            let c = c.clone();
            let latest = latest.clone();
            let stop = stop.clone();
            move || control::logs(c, latest, stop)
        });
        let worker = scope.spawn({
            let c = c.clone();
            let q = q.clone();
            let stop = stop.clone();
            let health = health.clone();
            move || deliver(c, q, stop, health)
        });
        let result = (|| -> Result<()> {
            let mut collector = Collector::new();
            while !stop.load(Ordering::Relaxed) {
                let started = Instant::now();
                let mut s = collector.collect(&c)?;
                let outcome = otlp::encode(&s, &c)
                    .context("metrics encoding failed")
                    .and_then(|payload| {
                        q.enqueue(s.at_ns, &payload, now())
                            .context("metrics queue persistence failed")
                    });
                match outcome {
                    Ok(stats) => {
                        health.enqueue_failed.store(false, Ordering::Relaxed);
                        eprintln!(
                            "{}",
                            serde_json::json!({"event":"metrics_queued","depth":stats.depth,"payload_bytes":stats.payload_bytes,"oldest_age_seconds":stats.oldest_age_seconds,"expired":stats.expired,"evicted":stats.evicted,"permanent":stats.permanent,"rejected_points":stats.rejected_points,"persistence_failures":health.persistence_failures.load(Ordering::Relaxed)})
                        )
                    }
                    Err(error) => {
                        health.enqueue_failed.store(true, Ordering::Relaxed);
                        let failures =
                            health.persistence_failures.fetch_add(1, Ordering::Relaxed) + 1;
                        eprintln!(
                            "{}",
                            serde_json::json!({"event":"metrics_persistence_failed","persistence_failures":failures,"error":format!("{error:#}")})
                        );
                    }
                }
                s.state.errors.truncate(63);
                if health.enqueue_failed.load(Ordering::Relaxed)
                    || health.delivery_failed.load(Ordering::Relaxed)
                {
                    s.state
                        .errors
                        .push("durable metrics persistence failed; delivery is degraded".into());
                }
                *latest
                    .lock()
                    .map_err(|_| anyhow::anyhow!("state worker failed"))? = Some(s.state);
                wait_for_collection(
                    started,
                    Duration::from_secs(c.interval_seconds),
                    &stop,
                    &[&worker, &state, &logs],
                )?;
            }
            Ok(())
        })();
        stop.store(true, Ordering::Relaxed);
        for handle in [worker, state, logs] {
            handle
                .join()
                .map_err(|_| anyhow::anyhow!("agent worker panicked"))??;
        }
        result
    })
}
fn wait_for_collection(
    started: Instant,
    interval: Duration,
    stop: &AtomicBool,
    workers: &[&std::thread::ScopedJoinHandle<'_, Result<()>>],
) -> Result<()> {
    while !stop.load(Ordering::Relaxed) {
        anyhow::ensure!(
            workers.iter().all(|worker| !worker.is_finished()),
            "agent worker stopped unexpectedly"
        );
        if started.elapsed() >= interval {
            break;
        }
        control::wait(stop, Duration::from_millis(100));
    }
    Ok(())
}

fn now() -> i64 {
    Utc::now().timestamp_nanos_opt().unwrap_or(i64::MAX)
}
fn deliver(c: Config, q: Arc<Outbox>, stop: Arc<AtomicBool>, health: Arc<Health>) -> Result<()> {
    let client = delivery::client()?;
    let mut backoff = 1u64;
    while !stop.load(Ordering::Relaxed) {
        let outcome = delivery::deliver_one(&client, &c, &q, now());
        health
            .delivery_failed
            .store(outcome.is_err(), Ordering::Relaxed);
        match outcome {
            Ok(Some(SendOutcome::Retry { after })) => {
                let jitter = uuid::Uuid::new_v4().as_bytes()[0] as u64 * 4;
                let delay = (Duration::from_secs(backoff) + Duration::from_millis(jitter))
                    .max(after.unwrap_or_default());
                eprintln!(
                    "{}",
                    serde_json::json!({"event":"metrics_delivery_retry","delay_ms":delay.as_millis()})
                );
                control::wait(&stop, delay);
                backoff = (backoff * 2).min(60);
            }
            Ok(Some(outcome)) => {
                if !matches!(outcome, SendOutcome::Accepted) {
                    eprintln!(
                        "{}",
                        serde_json::json!({"event":"metrics_rejected","outcome":format!("{outcome:?}")})
                    );
                }
                backoff = 1;
            }
            Ok(None) => control::wait(&stop, Duration::from_secs(1)),
            Err(error) => {
                let failures = health.persistence_failures.fetch_add(1, Ordering::Relaxed) + 1;
                eprintln!(
                    "{}",
                    serde_json::json!({"event":"queue_io_failed","persistence_failures":failures,"error":format!("{error:#}")})
                );
                control::wait(&stop, Duration::from_secs(1));
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn failed_worker_is_detected_after_collection_exceeds_interval() {
        std::thread::scope(|scope| {
            let worker = scope.spawn(|| anyhow::bail!("worker failed"));
            let deadline = Instant::now() + Duration::from_secs(2);
            while !worker.is_finished() {
                assert!(Instant::now() < deadline, "worker did not finish");
                std::thread::yield_now();
            }
            let result = wait_for_collection(
                Instant::now() - Duration::from_secs(2),
                Duration::from_secs(1),
                &AtomicBool::new(false),
                &[&worker],
            );
            assert!(
                result.is_err(),
                "failed worker went undetected after slow collection"
            );
        });
    }

    #[test]
    fn normal_shutdown_does_not_report_worker_failure() {
        std::thread::scope(|scope| {
            let worker = scope.spawn(|| Ok(()));
            assert!(
                wait_for_collection(
                    Instant::now(),
                    Duration::from_secs(1),
                    &AtomicBool::new(true),
                    &[&worker],
                )
                .is_ok()
            );
        });
    }
}
