use std::{
    fs::{File, OpenOptions},
    io::{Read, Write},
    sync::{Arc, Condvar, Mutex},
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

#[derive(Clone)]
enum Backend {
    Pty(Arc<PtyBackend>),
    Tty(Arc<TtyBackend>),
}

struct PtyBackend {
    master: Mutex<Box<dyn MasterPty + Send>>,
    reader: Mutex<Box<dyn Read + Send>>,
    writer: Mutex<Box<dyn Write + Send>>,
    child: Mutex<Box<dyn Child + Send + Sync>>,
    killer: Mutex<Box<dyn ChildKiller + Send + Sync>>,
}

struct TtyBackend {
    reader: Mutex<File>,
    writer: Mutex<File>,
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
        let writer = file.try_clone()?;
        Ok(Self {
            mode: Mode::Tty,
            source: path.to_string(),
            inner: Mutex::new(Inner {
                backend: Some(Backend::Tty(Arc::new(TtyBackend {
                    reader: Mutex::new(file),
                    writer: Mutex::new(writer),
                }))),
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
                backend: Some(Backend::Pty(Arc::new(PtyBackend {
                    master: Mutex::new(pair.master),
                    reader: Mutex::new(reader),
                    writer: Mutex::new(writer),
                    child: Mutex::new(child),
                    killer: Mutex::new(killer),
                }))),
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
        let backend = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .backend
            .as_ref()
            .cloned();
        match backend {
            Some(Backend::Pty(pty)) => pty
                .child
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .process_id()
                .unwrap_or(0),
            _ => 0,
        }
    }

    pub fn read(&self, buf: &mut [u8]) -> Result<usize> {
        let backend = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .backend
            .as_ref()
            .cloned();
        match backend {
            Some(Backend::Pty(pty)) => Ok(pty
                .reader
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .read(buf)?),
            Some(Backend::Tty(tty)) => Ok(tty
                .reader
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .read(buf)?),
            None => Err(anyhow!("terminal is closed")),
        }
    }

    pub fn write_input(&self, data: &[u8]) -> Result<()> {
        if data.is_empty() {
            return Ok(());
        }
        let backend = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .backend
            .as_ref()
            .cloned();
        match backend {
            Some(Backend::Pty(pty)) => {
                let mut writer = pty
                    .writer
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner());
                writer.write_all(data)?;
                writer.flush()?;
                Ok(())
            }
            Some(Backend::Tty(tty)) => {
                let mut writer = tty
                    .writer
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner());
                writer.write_all(data)?;
                writer.flush()?;
                Ok(())
            }
            None => Err(anyhow!("terminal is closed")),
        }
    }

    pub fn resize(&self, cols: u16, rows: u16) -> Result<()> {
        if cols == 0 || rows == 0 {
            return Ok(());
        }
        let backend = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .backend
            .as_ref()
            .cloned();
        match backend {
            Some(Backend::Pty(pty)) => {
                pty.master
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner())
                    .resize(PtySize {
                        rows,
                        cols,
                        pixel_width: 0,
                        pixel_height: 0,
                    })?;
                Ok(())
            }
            Some(Backend::Tty(tty)) => {
                #[cfg(unix)]
                {
                    use std::os::fd::AsRawFd;

                    let file = tty
                        .writer
                        .lock()
                        .unwrap_or_else(|poisoned| poisoned.into_inner());
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
        let backend = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .backend
            .as_ref()
            .cloned();
        match backend {
            Some(Backend::Pty(pty)) => loop {
                if pty
                    .child
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner())
                    .try_wait()?
                    .is_some()
                {
                    return Ok(());
                }
                std::thread::sleep(Duration::from_millis(25));
            },
            Some(Backend::Tty(_)) => {
                let mut guard = self
                    .inner
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner());
                while !guard.closed {
                    guard = self
                        .closed_cv
                        .wait(guard)
                        .unwrap_or_else(|poisoned| poisoned.into_inner());
                }
                Ok(())
            }
            None => Ok(()),
        }
    }

    pub fn close(&self) -> Result<()> {
        let backend = {
            let mut guard = self
                .inner
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner());
            if guard.closed {
                return Ok(());
            }
            let backend = guard.backend.take();
            guard.closed = true;
            self.closed_cv.notify_all();
            backend
        };
        if let Some(Backend::Pty(pty)) = backend {
            let _ = pty
                .killer
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .kill();
            let _ = pty
                .child
                .lock()
                .unwrap_or_else(|poisoned| poisoned.into_inner())
                .kill();
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::{
        io::{Read, Write},
        os::fd::AsFd,
        sync::{Arc, Mutex},
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

    #[test]
    fn concurrent_reader_does_not_block_input_writes() {
        let term = Arc::new(
            Terminal::start_command(
                "printf 'ready\\n'; while IFS= read -r line; do echo \"ECHO:$line\"; done",
            )
            .unwrap(),
        );

        let reader = Arc::clone(&term);
        let output = Arc::new(Mutex::new(String::new()));
        let output_reader = Arc::clone(&output);
        let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let stop_reader = Arc::clone(&stop);

        let handle = std::thread::spawn(move || {
            let deadline = Instant::now() + Duration::from_secs(4);
            while Instant::now() < deadline
                && !stop_reader.load(std::sync::atomic::Ordering::Relaxed)
            {
                let mut buf = [0_u8; 256];
                if let Ok(n) = reader.read(&mut buf) {
                    if n > 0 {
                        output_reader
                            .lock()
                            .unwrap()
                            .push_str(&String::from_utf8_lossy(&buf[..n]));
                    }
                }
            }
        });

        let deadline = Instant::now() + Duration::from_secs(4);
        while Instant::now() < deadline {
            if output.lock().unwrap().contains("ready") {
                break;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        assert!(output.lock().unwrap().contains("ready"));

        term.write_input(b"hello-from-concurrent\n").unwrap();

        let deadline = Instant::now() + Duration::from_secs(4);
        while Instant::now() < deadline {
            if output
                .lock()
                .unwrap()
                .contains("ECHO:hello-from-concurrent")
            {
                break;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        assert!(
            output
                .lock()
                .unwrap()
                .contains("ECHO:hello-from-concurrent")
        );

        stop.store(true, std::sync::atomic::Ordering::Relaxed);
        term.close().unwrap();
        let _ = handle.join();
        term.wait().unwrap();
    }
}
