// Test driver only: production configuration has no clock or crash controls.
use monitoring_agent::{
    Result,
    collect::Collection,
    config::Config,
    delivery, otlp,
    outbox::{Limits, Outbox, QueueIdentity},
};
use std::{
    io::{BufRead, Write},
    path::Path,
    time::Duration,
};
fn main() -> Result<()> {
    let args: Vec<_> = std::env::args().collect();
    let c = Config::load(Path::new(&args[1]))?;
    let q = Outbox::open(
        &c.queue_database,
        &QueueIdentity {
            host_id: c.host_id.clone(),
            metrics_url: c.metrics_url(),
        },
        Limits {
            retention: Duration::from_secs(c.queue_retention_seconds),
            max_bytes: c.queue_max_bytes,
        },
    )?;
    let client = delivery::client()?;
    for line in std::io::stdin().lock().lines() {
        let value: serde_json::Value = serde_json::from_str(&line?)?;
        let now = value["now_ns"]
            .as_i64()
            .ok_or_else(|| anyhow::anyhow!("missing test time"))?;
        let response = match value["op"].as_str() {
            Some("collect") => {
                serde_json::to_value(monitoring_agent::collect::Collector::new().collect(&c)?)?
            }
            Some("enqueue") => {
                let s: Collection = serde_json::from_value(value["collection"].clone())?;
                let body = otlp::encode(&s, &c)?;
                serde_json::to_value(q.enqueue(s.at_ns, &body, now)?)?
            }
            Some("stats") => serde_json::to_value(q.stats(now)?)?,
            Some("drain") => {
                serde_json::json!({"outcome":format!("{:?}",delivery::deliver_one(&client,&c,&q,now)?)})
            }
            _ => anyhow::bail!("unknown test command"),
        };
        println!("{response}");
        std::io::stdout().flush()?;
    }
    Ok(())
}
