pub mod agent;
pub mod collect;
pub mod config;
pub mod control;
pub mod delivery;
pub mod docker;
pub mod otlp;
pub mod outbox;
pub type Result<T> = anyhow::Result<T>;
