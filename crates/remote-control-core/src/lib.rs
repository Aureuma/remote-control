pub mod auth;
pub mod config;
pub mod flow;
pub mod runtime_state;

use std::ffi::OsString;

use anyhow::Result;

pub fn run_remote_control(_args: &[OsString]) -> Result<i32> {
    Ok(0)
}

pub fn run_safari_smoke(_args: &[OsString]) -> Result<i32> {
    Ok(0)
}
