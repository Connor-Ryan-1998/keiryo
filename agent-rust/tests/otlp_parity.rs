use monitoring_agent::{collect::*, config::Config, otlp};
use opentelemetry_proto::tonic::{
    collector::metrics::v1::ExportMetricsServiceRequest, metrics::v1::metric::Data,
};
use prost::Message;

#[test]
fn encoder_preserves_scope_raw_counters_container_resources_and_timestamps() {
    let c = Config {
        host_id: "node".into(),
        display_name: "Node".into(),
        ..Default::default()
    };
    let mut s = Collection::new(&c);
    s.at_ns = 1_000_000_000;
    s.add(
        "system.cpu.time",
        "s",
        20.0,
        &[("cpu", "all"), ("state", "idle")],
        true,
        1,
    );
    s.state.containers.push(Container {
        id: "abc".into(),
        name: "app".into(),
        ..Default::default()
    });
    s.observations.push(Observation {
        name: "container.cpu.utilization".into(),
        unit: "1".into(),
        value: 75.0,
        entity: "abc".into(),
        ..Default::default()
    });
    let req =
        ExportMetricsServiceRequest::decode(otlp::encode(&s, &c).unwrap().as_slice()).unwrap();
    assert_eq!(req.resource_metrics.len(), 2);
    let scope = &req.resource_metrics[0].scope_metrics[0];
    assert_eq!(scope.scope.as_ref().unwrap().name, "keiryo.agent");
    assert_eq!(scope.scope.as_ref().unwrap().version, "0.1.0");
    let Data::Sum(sum) = scope.metrics[0].data.as_ref().unwrap() else {
        panic!("counter must be a sum")
    };
    assert!(sum.is_monotonic);
    assert_eq!(sum.aggregation_temporality, 2);
    assert_eq!(sum.data_points[0].time_unix_nano, 1_000_000_000);
    assert_eq!(sum.data_points[0].start_time_unix_nano, 1);
    let Data::Gauge(g) = req.resource_metrics[1].scope_metrics[0].metrics[0]
        .data
        .as_ref()
        .unwrap()
    else {
        panic!()
    };
    assert_eq!(
        g.data_points[0].value,
        Some(opentelemetry_proto::tonic::metrics::v1::number_data_point::Value::AsDouble(75.0))
    );
    assert!(
        req.resource_metrics[1]
            .resource
            .as_ref()
            .unwrap()
            .attributes
            .iter()
            .any(|v| v.key == "container.id")
    );
}

#[test]
fn invalid_observations_cannot_enter_the_wire_queue() {
    let c = Config::default();
    let mut s = Collection::new(&c);
    s.add("system.uptime", "s", f64::NAN, &[], false, 0);
    assert!(otlp::encode(&s, &c).is_err());
}
