use std::{
    fs::{File, OpenOptions},
    io::{Read, Write},
    sync::{Condvar, Mutex},
    time::Duration,
};

use anyhow::{Result, anyhow};
use portable_pty::{Child, ChildKiller, CommandBuilder, MasterPty, PtySize, native_pty_system};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    Attach,
    Cmd,
    Tty,
}

enum Backend {
    Pty {
        master: Box<dyn MasterPty + Send>,
        reader: Box<dyn Read + Send>,
        writer: Box<dyn Write + Send>,
        child: Box<dyn Child + Send + Sync>,
        killer: Box<dyn ChildKiller + Send + Sync>,
    },
    Tty {
        file: File,
    },
}

struct Inner {
    backend: Option<Backend>,
    closed: bool,
}

pub struct Terminal {
    mode: Mode,
    source: String,
    inner: Mutex<Inner>,
    closed_cv: Condvar,
}

impl Terminal {
    pub fn start_attach(tmux_session: &str) -> Result<Self> {
        let tmux_session = tmux_session.trim();
        if tmux_session.is_empty() {
            return Err(anyhow!("tmux session is required"));
        }
        crate::tmux::ensure_installed()?;
        let mut cmd = CommandBuilder::new("tmux");
        cmd.arg("attach-session");
        cmd.arg("-t");
        cmd.arg(tmux_session);
        Self::start_with_builder(Mode::Attach, tmux_session.to_string(), cmd)
    }

    pub fn start_command(command: &str) -> Result<Self> {
        let command = command.trim();
        if command.is_empty() {
            return Err(anyhow!("command is required"));
        }
        #[cfg(unix)]
        let cmd = {
            let mut cmd = CommandBuilder::new("/bin/sh");
            cmd.arg("-lc");
            cmd.arg(command);
            cmd
        };
        #[cfg(windows)]
        let cmd = {
            let mut cmd = CommandBuilder::new("cmd");
            cmd.arg("/C");
            cmd.arg(command);
            cmd
        };
        Self::start_with_builder(Mode::Cmd, command.to_string(), cmd)
    }

    pub fn start_tty_path(path: &str) -> Result<Self> {
        let path = path.trim();
        if path.is_empty() {
            return Err(anyhow!("tty path is required"));
        }
        let file = OpenOptions::new().read(true).write(true).open(path)?;
        Ok(Self {
            mode: Mode::Tty,
            source: path.to_string(),
            inner: Mutex::new(Inner {
                backend: Some(Backend::Tty { file }),
                closed: false,
            }),
            closed_cv: Condvar::new(),
        })
    }

    fn start_with_builder(mode: Mode, source: String, cmd: CommandBuilder) -> Result<Self> {
        let pty_system = native_pty_system();
        let pair = pty_system.openpty(PtySize {
            rows: 24,
            cols: 80,
            pixel_width: 0,
            pixel_height: 0,
        })?;
        let reader = pair.master.try_clone_reader()?;
        let writer = pair.master.take_writer()?;
        let child = pair.slave.spawn_command(cmd)?;
        let killer = child.clone_killer();
        Ok(Self {
            mode,
            source,
            inner: Mutex::new(Inner {
                backend: Some(Backend::Pty {
                    master: pair.master,
                    reader,
                    writer,
                    child,
                    killer,
                }),
                closed: false,
            }),
            closed_cv: Condvar::new(),
        })
    }

    pub fn mode(&self) -> Mode {
        self.mode
    }

    pub fn source(&self) -> &str {
        &self.source
    }

    pub fn pid(&self) -> u32 {
        let mut guard = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        match guard.backend.as_mut() {
            Some(Backend::Pty { child, .. }) => child.process_id().unwrap_or(0),
            _ => 0,
        }
    }

    pub fn read(&self, buf: &mut [u8]) -> Result<usize> {
        let mut guard = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        match guard.backend.as_mut() {
            Some(Backend::Pty { reader, .. }) => Ok(reader.read(buf)?),
            Some(Backend::Tty { file }) => Ok(file.read(buf)?),
            None => Err(anyhow!("terminal is closed")),
        }
    }

    pub fn write_input(&self, data: &[u8]) -> Result<()> {
        if data.is_empty() {
            return Ok(());
        }
        let mut guard = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        match guard.backend.as_mut() {
            Some(Backend::Pty { writer, .. }) => {
                writer.write_all(data)?;
                writer.flush()?;
                Ok(())
            }
            Some(Backend::Tty { file }) => {
                file.write_all(data)?;
                file.flush()?;
                Ok(())
            }
            None => Err(anyhow!("terminal is closed")),
        }
    }

    pub fn resize(&self, cols: u16, rows: u16) -> Result<()> {
        if cols == 0 || rows == 0 {
            return Ok(());
        }
        let mut guard = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        match guard.backend.as_mut() {
            Some(Backend::Pty { master, .. }) => {
                master.resize(PtySize {
                    rows,
                    cols,
                    pixel_width: 0,
                    pixel_height: 0,
                })?;
                Ok(())
            }
            Some(Backend::Tty { file }) => {
                #[cfg(unix)]
                {
                    use std::os::fd::AsRawFd;

                    let winsize = nix::libc::winsize {
                        ws_row: rows,
                        ws_col: cols,
                        ws_xpixel: 0,
                        ws_ypixel: 0,
                    };
                    let rc = unsafe {
                        nix::libc::ioctl(file.as_raw_fd(), nix::libc::TIOCSWINSZ, &winsize)
                    };
                    if rc != 0 {
                        return Err(std::io::Error::last_os_error().into());
                    }
                }
                Ok(())
            }
            None => Err(anyhow!("terminal is closed")),
        }
    }

    pub fn wait(&self) -> Result<()> {
        loop {
            let mut guard = self
                .inner
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            match guard.backend.as_mut() {
                Some(Backend::Pty { child, .. }) => {
                    if child.try_wait()?.is_some() {
                        return Ok(());
                    }
                    drop(guard);
                    std::thread::sleep(Duration::from_millis(25));
                }
                Some(Backend::Tty { .. }) => {
                    while !guard.closed {
                        guard = self
                            .closed_cv
                            .wait(guard)
                            .unwrap_or_else(|poisoned| poisoned.into_inner());
                    }
                    return Ok(());
                }
                None => return Ok(()),
            }
        }
    }

    pub fn close(&self) -> Result<()> {
        let mut guard = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        if guard.closed {
            return Ok(());
        }
        if let Some(backend) = guard.backend.take() {
            match backend {
                Backend::Pty {
                    mut child,
                    mut killer,
                    ..
                } => {
                    let _ = killer.kill();
                    let _ = child.kill();
                }
                Backend::Tty { .. } => {}
            }
        }
        guard.closed = true;
        self.closed_cv.notify_all();
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::{
        io::{Read, Write},
        os::fd::AsFd,
        time::{Duration, Instant},
    };

    use nix::{pty::openpty, unistd::ttyname};

    use crate::session::{Mode, Terminal};

    #[test]
    fn start_tty_path_read_write_and_wait() {
        let pair = openpty(None, None).unwrap();
        let tty_path = ttyname(pair.slave.as_fd()).unwrap();
        let term = Terminal::start_tty_path(tty_path.to_string_lossy().as_ref()).unwrap();

        assert_eq!(term.mode(), Mode::Tty);
        assert_eq!(term.source(), tty_path.to_string_lossy());

        let mut master = std::fs::File::from(pair.master);
        master.write_all(b"hello\n").unwrap();

        let mut buf = [0_u8; 32];
        let n = term.read(&mut buf).unwrap();
        let payload = String::from_utf8_lossy(&buf[..n]).replace("\r\n", "\n");
        assert_eq!(payload, "hello\n");

        term.write_input(b"echo\n").unwrap();
        let deadline = Instant::now() + Duration::from_secs(2);
        let mut accumulated = String::new();
        while Instant::now() < deadline {
            let mut read_buf = [0_u8; 64];
            if let Ok(n) = master.read(&mut read_buf) {
                if n > 0 {
                    accumulated
                        .push_str(&String::from_utf8_lossy(&read_buf[..n]).replace("\r\n", "\n"));
                    if accumulated.contains("echo") {
                        break;
                    }
                }
            }
        }
        assert!(accumulated.contains("echo"));

        let waiter = std::thread::scope(|scope| {
            let handle = scope.spawn(|| term.wait());
            std::thread::sleep(Duration::from_millis(200));
            term.close().unwrap();
            handle.join().unwrap()
        });
        waiter.unwrap();
    }

    #[test]
    fn start_command_reads_output_and_accepts_input() {
        let term = Terminal::start_command(
            "printf 'ready\\n'; while IFS= read -r line; do echo \"ECHO:$line\"; done",
        )
        .unwrap();
        assert_eq!(term.mode(), Mode::Cmd);
        assert!(term.pid() > 0);

        let deadline = Instant::now() + Duration::from_secs(4);
        let mut output = String::new();
        while Instant::now() < deadline {
            let mut buf = [0_u8; 128];
            let n = term.read(&mut buf).unwrap_or(0);
            if n > 0 {
                output.push_str(&String::from_utf8_lossy(&buf[..n]));
                if output.contains("ready") {
                    break;
                }
            }
        }
        assert!(output.contains("ready"));

        term.write_input(b"hello-from-rust\n").unwrap();
        let deadline = Instant::now() + Duration::from_secs(4);
        while Instant::now() < deadline && !output.contains("ECHO:hello-from-rust") {
            let mut buf = [0_u8; 256];
            let n = term.read(&mut buf).unwrap_or(0);
            if n > 0 {
                output.push_str(&String::from_utf8_lossy(&buf[..n]));
            }
        }
        assert!(output.contains("ECHO:hello-from-rust"));

        term.close().unwrap();
        term.wait().unwrap();
    }
}
