use crate::{
    Result,
    collect::Collection,
    config::{Config, valid_id},
};
use anyhow::ensure;
use opentelemetry_proto::tonic::{
    collector::metrics::v1::ExportMetricsServiceRequest,
    common::v1::{AnyValue, InstrumentationScope, KeyValue, any_value},
    metrics::v1::{
        Gauge, Metric, NumberDataPoint, ResourceMetrics, ScopeMetrics, Sum, metric,
        number_data_point,
    },
    resource::v1::Resource,
};
use prost::Message;
use std::collections::BTreeMap;

fn attributes(values: impl IntoIterator<Item = (String, String)>) -> Vec<KeyValue> {
    values
        .into_iter()
        .map(|(key, value)| KeyValue {
            key,
            value: Some(AnyValue {
                value: Some(any_value::Value::StringValue(value)),
            }),
            ..Default::default()
        })
        .collect()
}

pub fn encode(s: &Collection, c: &Config) -> Result<Vec<u8>> {
    ensure!(
        valid_id(&c.host_id) && s.at_ns > 0 && s.observations.len() <= 2048,
        "invalid OTLP host/time or point limit"
    );
    let mut groups: BTreeMap<&str, Vec<Metric>> = BTreeMap::new();
    for o in &s.observations {
        ensure!(
            o.value.is_finite()
                && o.value >= 0.0
                && o.value <= 1e30
                && o.start_ns >= 0
                && o.start_ns <= s.at_ns
                && o.attributes.len() <= 32
                && o.attributes
                    .iter()
                    .all(|(k, v)| !k.is_empty() && k.len() <= 128 && v.len() <= 512),
            "invalid metric point"
        );
        ensure!(
            o.entity.is_empty() || valid_id(&o.entity),
            "invalid container identity"
        );
        let point = NumberDataPoint {
            attributes: attributes(o.attributes.clone()),
            time_unix_nano: s.at_ns as u64,
            start_time_unix_nano: o.start_ns as u64,
            value: Some(number_data_point::Value::AsDouble(o.value)),
            ..Default::default()
        };
        let metrics = groups.entry(&o.entity).or_default();
        if let Some(m) = metrics.iter_mut().find(|m| m.name == o.name) {
            match m.data.as_mut().unwrap() {
                metric::Data::Gauge(g) => g.data_points.push(point),
                metric::Data::Sum(v) => v.data_points.push(point),
                _ => unreachable!(),
            }
        } else {
            metrics.push(Metric {
                name: o.name.clone(),
                unit: o.unit.clone(),
                data: Some(if o.counter {
                    metric::Data::Sum(Sum {
                        is_monotonic: true,
                        aggregation_temporality: 2,
                        data_points: vec![point],
                    })
                } else {
                    metric::Data::Gauge(Gauge {
                        data_points: vec![point],
                    })
                }),
                ..Default::default()
            });
        }
    }
    let mut req = ExportMetricsServiceRequest::default();
    for (entity, metrics) in groups {
        let mut values = BTreeMap::from([
            ("host.id".into(), c.host_id.clone()),
            ("host.name".into(), c.display_name.clone()),
        ]);
        if !entity.is_empty() {
            values.insert("container.id".into(), entity.into());
            values.insert(
                "container.name".into(),
                s.state
                    .containers
                    .iter()
                    .find(|v| v.id == entity)
                    .map(|v| v.name.clone())
                    .unwrap_or_default(),
            );
        }
        req.resource_metrics.push(ResourceMetrics {
            resource: Some(Resource {
                attributes: attributes(values),
                ..Default::default()
            }),
            scope_metrics: vec![ScopeMetrics {
                scope: Some(InstrumentationScope {
                    name: "keiryo.agent".into(),
                    version: "0.1.0".into(),
                    ..Default::default()
                }),
                metrics,
                ..Default::default()
            }],
            ..Default::default()
        });
    }
    let body = req.encode_to_vec();
    ensure!(
        body.len() <= 2 * 1024 * 1024,
        "OTLP batch exceeds request limit"
    );
    Ok(body)
}
