use std::{process::Command, sync::OnceLock};

use anyhow::{Result, anyhow};

#[derive(Debug)]
pub struct Handle {
    child: std::process::Child,
}

impl Handle {
    pub fn pid(&self) -> u32 {
        self.child.id()
    }

    pub fn stop(&mut self) -> Result<()> {
        match self.child.kill() {
            Ok(()) => {}
            Err(err) if err.kind() == std::io::ErrorKind::InvalidInput => {}
            Err(err) => return Err(err.into()),
        }
        let _ = self.child.wait();
        Ok(())
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        let _ = self.stop();
    }
}

pub fn start() -> Result<Option<Handle>> {
    if std::env::consts::OS != "macos" {
        return Ok(None);
    }
    let Some(binary) = find_caffeinate() else {
        return Err(anyhow!("caffeinate not found"));
    };
    let child = Command::new(binary).args(["-dimsu"]).spawn()?;
    Ok(Some(Handle { child }))
}

fn find_caffeinate() -> Option<&'static str> {
    static PATH: OnceLock<bool> = OnceLock::new();
    if *PATH.get_or_init(|| {
        Command::new("sh")
            .args(["-lc", "command -v caffeinate >/dev/null 2>&1"])
            .status()
            .map(|status| status.success())
            .unwrap_or(false)
    }) {
        Some("caffeinate")
    } else {
        None
    }
}
