package websocket

type flowEvent int

const (
	flowEventNone flowEvent = iota
	flowEventPause
	flowEventResume
)

type flowController struct {
	low     int64
	high    int64
	pending int64
	paused  bool
}

func newFlowController(low, high int64) flowController {
	if low <= 0 {
		low = 512 * 1024
	}
	if high <= 0 {
		high = 2 * 1024 * 1024
	}
	if low > high {
		low = high / 2
		if low <= 0 {
			low = 1
		}
	}
	return flowController{low: low, high: high}
}

func (f *flowController) onSent(n int) flowEvent {
	if n <= 0 {
		return flowEventNone
	}
	f.pending += int64(n)
	if !f.paused && f.pending > f.high {
		f.paused = true
		return flowEventPause
	}
	return flowEventNone
}

func (f *flowController) onAck(n int64) flowEvent {
	if n <= 0 {
		return flowEventNone
	}
	f.pending -= n
	if f.pending < 0 {
		f.pending = 0
	}
	if f.paused && f.pending <= f.low {
		f.paused = false
		return flowEventResume
	}
	return flowEventNone
}

func (f *flowController) reset() {
	f.pending = 0
	f.paused = false
}
