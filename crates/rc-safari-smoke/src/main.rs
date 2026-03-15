use std::{ffi::OsString, process::ExitCode};

fn main() -> ExitCode {
    let args: Vec<OsString> = std::env::args_os().skip(1).collect();
    match remote_control_core::run_safari_smoke(&args) {
        Ok(code) => ExitCode::from(code as u8),
        Err(err) => {
            eprintln!("error: {err:#}");
            ExitCode::FAILURE
        }
    }
}
