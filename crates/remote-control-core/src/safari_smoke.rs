use std::{
    ffi::OsString,
    fs,
    path::PathBuf,
    process::{Command, Stdio},
};

use anyhow::{Result, anyhow};

const USAGE: &str = "rc-safari-smoke [--scenarios readwrite,readonly,access-code,no-token] [--verbose] [--keep-artifacts]\n";

pub fn run(args: &[OsString]) -> Result<i32> {
    let args = args
        .iter()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    if args.iter().any(|arg| arg == "--help" || arg == "-h") {
        print!("{USAGE}");
        return Ok(0);
    }

    let scenarios = value_for_flag(&args, "--scenarios")
        .unwrap_or_else(|| "readwrite,readonly,access-code,no-token".to_string());
    let grep = scenario_grep_pattern(&scenarios)?;
    let verbose = args.iter().any(|arg| arg == "--verbose");
    let keep_artifacts = args.iter().any(|arg| arg == "--keep-artifacts");
    let repo_root = repo_root();

    let mut command = Command::new("npm");
    command.args(["run", "test:browser", "--", "--project=webkit"]);
    if !grep.is_empty() {
        command.args(["--grep", &grep]);
    }
    command.current_dir(&repo_root);
    if verbose {
        command.env("PWDEBUG", "0");
    }
    command.stdin(Stdio::null());
    command.stdout(Stdio::inherit());
    command.stderr(Stdio::inherit());

    let status = command.status()?;
    if status.success() {
        if !keep_artifacts {
            let _ = fs::remove_dir_all(repo_root.join("test-results"));
        }
        return Ok(0);
    }
    Err(anyhow!(
        "webkit smoke failed{}",
        status
            .code()
            .map(|code| format!(" with exit code {code}"))
            .unwrap_or_default()
    ))
}

fn repo_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(|path| path.parent())
        .unwrap()
        .to_path_buf()
}

fn scenario_grep_pattern(csv: &str) -> Result<String> {
    let mut names = Vec::new();
    for raw in csv.split(',') {
        let trimmed = raw.trim();
        if trimmed.is_empty() {
            continue;
        }
        let test_name = match trimmed {
            "readwrite" => "connects and streams output in readwrite mode",
            "readonly" => "shows read-only state and blocks browser input writes",
            "access-code" => "requires access code when configured",
            "no-token" => "supports no-token-in-url by prompting for token",
            other => {
                return Err(anyhow!(
                    "unknown scenario \"{other}\" (allowed: readwrite, readonly, access-code, no-token)"
                ));
            }
        };
        if !names.iter().any(|existing| *existing == test_name) {
            names.push(test_name);
        }
    }
    if names.is_empty() {
        return Err(anyhow!("no scenarios selected"));
    }
    Ok(names.join("|"))
}

fn value_for_flag(args: &[String], flag: &str) -> Option<String> {
    args.windows(2)
        .find(|window| window[0] == flag)
        .map(|window| window[1].clone())
}

#[cfg(test)]
mod tests {
    use crate::safari_smoke::scenario_grep_pattern;

    #[test]
    fn scenario_grep_pattern_matches_supported_scenarios() {
        assert_eq!(
            scenario_grep_pattern("readwrite,readonly,readwrite").unwrap(),
            "connects and streams output in readwrite mode|shows read-only state and blocks browser input writes"
        );
        assert!(
            scenario_grep_pattern("access-code,no-token")
                .unwrap()
                .contains("requires access code when configured")
        );
    }

    #[test]
    fn scenario_grep_pattern_rejects_empty_and_unknown_values() {
        assert!(scenario_grep_pattern(" ").is_err());
        assert!(scenario_grep_pattern("bogus").is_err());
    }
}
