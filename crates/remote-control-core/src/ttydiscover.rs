use std::process::Command;

use anyhow::{Result, anyhow};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Candidate {
    pub pid: i32,
    pub tty: String,
    pub command: String,
    pub args: String,
}

pub fn list() -> Result<Vec<Candidate>> {
    if which("ps").is_none() {
        return Err(anyhow!("ps not found in PATH"));
    }
    let output = Command::new("ps")
        .args(["-eo", "pid=,tty=,comm=,args="])
        .output()?;
    if !output.status.success() {
        return Err(anyhow!("ps command failed"));
    }
    let mut candidates = parse_ps_output(&String::from_utf8_lossy(&output.stdout));
    candidates.sort_by(|left, right| left.tty.cmp(&right.tty).then(left.pid.cmp(&right.pid)));
    Ok(candidates)
}

pub fn parse_ps_output(raw: &str) -> Vec<Candidate> {
    raw.replace("\r\n", "\n")
        .lines()
        .filter_map(|line| {
            let line = line.trim();
            if line.is_empty() {
                return None;
            }
            let fields = line.split_whitespace().collect::<Vec<_>>();
            if fields.len() < 3 {
                return None;
            }
            let pid = fields[0].parse::<i32>().ok()?;
            if pid <= 0 {
                return None;
            }
            let tty = fields[1].trim();
            if tty.is_empty() || matches!(tty, "?" | "-") {
                return None;
            }
            let command = fields[2].trim();
            let args = if fields.len() > 3 {
                fields[3..].join(" ")
            } else {
                String::new()
            };
            Some(Candidate {
                pid,
                tty: tty_path(tty),
                command: command.to_string(),
                args,
            })
        })
        .collect()
}

pub fn tty_path(tty: &str) -> String {
    let tty = tty.trim();
    if tty.is_empty() || tty.starts_with('/') {
        return tty.to_string();
    }
    format!("/dev/{tty}")
}

fn which(binary: &str) -> Option<String> {
    let output = Command::new("sh")
        .args(["-lc", &format!("command -v {binary}")])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let value = String::from_utf8_lossy(&output.stdout).trim().to_string();
    (!value.is_empty()).then_some(value)
}

#[cfg(test)]
mod tests {
    #[test]
    fn parse_ps_output_matches_go_behavior() {
        let raw = "\n123 pts/1 bash -i\n124 ? cron /usr/sbin/cron -f\nabc pts/2 zsh -l\n222 tty1 login -\n333 - kworker/u16:2\n";
        let candidates = crate::ttydiscover::parse_ps_output(raw);
        assert_eq!(candidates.len(), 2);
        assert_eq!(candidates[0].pid, 123);
        assert_eq!(candidates[0].tty, "/dev/pts/1");
        assert_eq!(candidates[0].command, "bash");
        assert_eq!(candidates[1].pid, 222);
        assert_eq!(candidates[1].tty, "/dev/tty1");
        assert_eq!(candidates[1].command, "login");
    }

    #[test]
    fn tty_path_matches_go_behavior() {
        assert_eq!(crate::ttydiscover::tty_path("pts/3"), "/dev/pts/3");
        assert_eq!(crate::ttydiscover::tty_path("/dev/tty7"), "/dev/tty7");
    }
}
