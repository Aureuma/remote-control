use std::ffi::OsString;

pub fn run_remote_control(_args: &[OsString]) -> anyhow::Result<i32> {
    Ok(0)
}

pub fn run_safari_smoke(_args: &[OsString]) -> anyhow::Result<i32> {
    Ok(0)
}

#[cfg(test)]
mod tests {
    use std::ffi::OsString;

    #[test]
    fn remote_control_stub_runs() {
        let code = crate::run_remote_control(&[OsString::from("status")]).unwrap();
        assert_eq!(code, 0);
    }
}
