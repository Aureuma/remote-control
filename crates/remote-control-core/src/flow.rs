#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FlowEvent {
    None,
    Pause,
    Resume,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FlowController {
    pub low: i64,
    pub high: i64,
    pub pending: i64,
    pub paused: bool,
}

impl FlowController {
    pub fn new(low: i64, high: i64) -> Self {
        let mut low = if low <= 0 { 512 * 1024 } else { low };
        let high = if high <= 0 { 2 * 1024 * 1024 } else { high };
        if low > high {
            low = (high / 2).max(1);
        }
        Self {
            low,
            high,
            pending: 0,
            paused: false,
        }
    }

    pub fn on_sent(&mut self, bytes: usize) -> FlowEvent {
        if bytes == 0 {
            return FlowEvent::None;
        }
        self.pending += bytes as i64;
        if !self.paused && self.pending > self.high {
            self.paused = true;
            return FlowEvent::Pause;
        }
        FlowEvent::None
    }

    pub fn on_ack(&mut self, bytes: i64) -> FlowEvent {
        if bytes <= 0 {
            return FlowEvent::None;
        }
        self.pending -= bytes;
        if self.pending < 0 {
            self.pending = 0;
        }
        if self.paused && self.pending <= self.low {
            self.paused = false;
            return FlowEvent::Resume;
        }
        FlowEvent::None
    }

    pub fn reset(&mut self) {
        self.pending = 0;
        self.paused = false;
    }
}

#[cfg(test)]
mod tests {
    use crate::flow::{FlowController, FlowEvent};

    #[test]
    fn flow_controller_pauses_and_resumes() {
        let mut flow = FlowController::new(10, 20);
        assert_eq!(flow.on_sent(8), FlowEvent::None);
        assert!(!flow.paused);
        assert_eq!(flow.on_sent(15), FlowEvent::Pause);
        assert!(flow.paused);
        assert_eq!(flow.on_ack(5), FlowEvent::None);
        assert!(flow.paused);
        assert_eq!(flow.on_ack(20), FlowEvent::Resume);
        assert!(!flow.paused);
    }

    #[test]
    fn flow_controller_resets_and_clamps() {
        let mut flow = FlowController::new(100, 50);
        assert!(flow.low > 0);
        assert!(flow.high > 0);
        assert!(flow.low <= flow.high);
        let _ = flow.on_sent(1000);
        assert!(flow.pending > 0);
        let _ = flow.on_ack(2000);
        assert_eq!(flow.pending, 0);
        let _ = flow.on_sent(100);
        flow.reset();
        assert_eq!(flow.pending, 0);
        assert!(!flow.paused);
    }
}
