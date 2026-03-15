pub mod app;
pub mod auth;
pub mod config;
pub mod flow;
pub mod runtime_state;
pub mod session;
pub mod tmux;
pub mod ttydiscover;
pub mod tunnel_cloudflare;
pub mod websocket_support;

#[cfg(test)]
pub(crate) mod test_support;

use std::ffi::OsString;

use anyhow::Result;

pub fn run_remote_control(args: &[OsString]) -> Result<i32> {
    app::run(args)
}

pub fn run_safari_smoke(_args: &[OsString]) -> Result<i32> {
    Ok(0)
}
