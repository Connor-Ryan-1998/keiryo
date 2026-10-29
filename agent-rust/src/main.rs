use monitoring_agent::{Result, agent, config::Config};
use std::{
    path::Path,
    sync::{Arc, atomic::AtomicBool},
};
fn run() -> Result<()> {
    let args: Vec<_> = std::env::args().skip(1).collect();
    anyhow::ensure!(
        args.len() == 3 && args[0] == "agent" && args[1] == "-config",
        "usage: monitoring-agent agent -config path.json"
    );
    let c = Config::load(Path::new(&args[2]))?;
    let stop = Arc::new(AtomicBool::new(false));
    for signal in [signal_hook::consts::SIGTERM, signal_hook::consts::SIGINT] {
        signal_hook::flag::register(signal, stop.clone())?;
    }
    agent::run(c, stop)
}
fn main() {
    if let Err(error) = run() {
        if let Some(json) = error.downcast_ref::<serde_json::Error>() {
            // Serde errors can quote configuration values; report location only.
            eprintln!(
                "monitoring agent stopped: invalid JSON data at line {} column {}",
                json.line(),
                json.column()
            );
        } else {
            eprintln!("monitoring agent stopped: {error:#}");
        }
        std::process::exit(1);
    }
}
