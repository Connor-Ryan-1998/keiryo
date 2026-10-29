use crate::{Result, config::Config};
use opentelemetry_proto::tonic::collector::metrics::v1::ExportMetricsServiceResponse;
use prost::Message;
use reqwest::blocking::Client;
use std::io::Read;
use std::time::Duration;
#[derive(Debug, PartialEq)]
pub enum SendOutcome {
    Accepted,
    Partial { rejected: u64 },
    Retry { after: Option<Duration> },
    Permanent { status: u16 },
}
pub fn client() -> Result<Client> {
    Ok(Client::builder()
        .redirect(reqwest::redirect::Policy::none())
        .timeout(Duration::from_secs(20))
        .no_proxy()
        .build()?)
}
pub fn send(client: &Client, c: &Config, body: &[u8]) -> SendOutcome {
    let retry = SendOutcome::Retry { after: None };
    let Ok(token) = crate::config::read_secret(&c.credential_file) else {
        return retry;
    };
    let Ok(response) = client
        .post(c.metrics_url())
        .bearer_auth(token)
        .header("X-Host-ID", &c.host_id)
        .header("Content-Type", "application/x-protobuf")
        .body(body.to_vec())
        .send()
    else {
        return retry;
    };
    let status = response.status().as_u16();
    if [429, 502, 503, 504].contains(&status) {
        let after = response
            .headers()
            .get("Retry-After")
            .and_then(|v| v.to_str().ok())
            .and_then(|v| {
                if let Ok(seconds) = v.parse::<u64>() {
                    Some(Duration::from_secs(seconds.min(300)))
                } else {
                    chrono::DateTime::parse_from_rfc2822(v).ok().map(|t| {
                        Duration::from_secs(
                            (t.timestamp() - chrono::Utc::now().timestamp()).clamp(0, 300) as u64,
                        )
                    })
                }
            });
        return SendOutcome::Retry { after };
    }
    if status != 200 {
        return SendOutcome::Permanent { status };
    }
    let content = response
        .headers()
        .get("Content-Type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .to_string();
    let mut data = Vec::new();
    if response
        .take(4 * 1024 * 1024 + 1)
        .read_to_end(&mut data)
        .is_err()
        || data.len() > 4 * 1024 * 1024
    {
        return retry;
    }
    let response = match content.as_str() {
        "application/x-protobuf" => ExportMetricsServiceResponse::decode(data.as_slice()).ok(),
        "application/json" => decode_json_response(&data),
        _ => None,
    };
    let Some(response) = response else {
        return retry;
    };
    match response.partial_success {
        Some(p) if p.rejected_data_points >= 0 => SendOutcome::Partial {
            rejected: p.rejected_data_points as u64,
        },
        Some(_) => retry,
        None => SendOutcome::Accepted,
    }
}

fn decode_json_response(data: &[u8]) -> Option<ExportMetricsServiceResponse> {
    let mut value: serde_json::Value = serde_json::from_slice(data).ok()?;
    // Generated serde uses i64, while OTLP protobuf JSON permits decimal strings.
    let count = value
        .get_mut("partialSuccess")
        .and_then(|p| p.get_mut("rejectedDataPoints"));
    if let Some(count) = count
        && let Some(text) = count.as_str()
    {
        *count = serde_json::json!(text.parse::<i64>().ok()?);
    }
    serde_json::from_value(value).ok()
}

pub fn deliver_one(
    client: &Client,
    c: &Config,
    q: &crate::outbox::Outbox,
    now: i64,
) -> Result<Option<SendOutcome>> {
    let Some(batch) = q.oldest(now)? else {
        return Ok(None);
    };
    let result = send(client, c, &batch.payload);
    match result {
        SendOutcome::Accepted => q.ack(batch.sequence)?,
        SendOutcome::Partial { rejected } => q.finish(batch.sequence, false, rejected)?,
        SendOutcome::Permanent { .. } => q.finish(batch.sequence, true, 0)?,
        SendOutcome::Retry { .. } => {}
    }
    Ok(Some(result))
}
