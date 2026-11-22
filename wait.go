package gaal

import (
	"errors"
	"time"
)

// Waiter is implemented by streams whose output arrives asynchronously, so a
// caller can sleep instead of polling Receive.
type Waiter interface {
	// Wait returns when output may be ready or the timeout passes.
	Wait(timeout time.Duration)
}

// ErrTimeout is returned by ReceiveWait when no output arrived in time.
var ErrTimeout = errors.New("gaal: timed out waiting for output")

// pollInterval paces ReceiveWait on streams that are not Waiters.
const pollInterval = 50 * time.Microsecond

// ReceiveWait is Receive that waits up to timeout for output.
func ReceiveWait[In, Out any](s Stream[In, Out], timeout time.Duration) (Out, error) {
	var deadline time.Time
	for {
		out, err := s.Receive()
		if err != ErrAgain {
			return out, err
		}
		now := time.Now()
		if deadline.IsZero() {
			deadline = now.Add(timeout)
		}
		left := deadline.Sub(now)
		if left <= 0 {
			return out, ErrTimeout
		}
		if w, ok := any(s).(Waiter); ok {
			w.Wait(left)
		} else {
			time.Sleep(min(left, pollInterval))
		}
	}
}

// Wait implements Waiter.
func (f *failover[In, Out]) Wait(timeout time.Duration) {
	if w, ok := any(f.cur).(Waiter); ok && !f.closed {
		w.Wait(timeout)
		return
	}
	time.Sleep(min(timeout, pollInterval))
}

// CodecConfigurer is implemented by audio encoders whose stream needs
// out-of-band setup, such as AAC's AudioSpecificConfig.
type CodecConfigurer interface {
	// CodecConfig returns the setup bytes for AudioConfig.Extra, or nil.
	CodecConfig() []byte
}

// CodecConfig implements CodecConfigurer.
func (f *failover[In, Out]) CodecConfig() []byte {
	if c, ok := any(f.cur).(CodecConfigurer); ok && !f.closed {
		return c.CodecConfig()
	}
	return nil
}
