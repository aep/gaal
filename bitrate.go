package gaal

import (
	"errors"
	"fmt"
)

// BitrateSetter is implemented by video encoders whose target bitrate can
// change while they run, for example to follow a receiver's bandwidth
// estimate. The change takes effect from the next frame sent, without a new
// keyframe and without reopening the codec.
//
// SetBitrate returns ErrInvalid for a bitrate of zero or less, and an error
// that matches errors.ErrUnsupported when the backend cannot change its
// bitrate live. A stream from NewVideoEncoder always implements it and keeps
// the new bitrate for any backend it fails over to, even when the current
// backend reports ErrUnsupported.
type BitrateSetter interface {
	SetBitrate(bps int) error
}

// errBitrateUnsupported is returned by backends that cannot change the rate.
func errBitrateUnsupported(backend string) error {
	return fmt.Errorf("gaal: %s cannot change its bitrate while running: %w", backend, errors.ErrUnsupported)
}

func checkBitrate(bps int) error {
	if bps <= 0 {
		return fmt.Errorf("%w: bitrate %d must be positive", ErrInvalid, bps)
	}
	return nil
}

// SetBitrate implements BitrateSetter.
func (f *failover[In, Out]) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if err := f.ready(); err != nil {
		return err
	}
	if f.onBitrate == nil {
		return errBitrateUnsupported(f.cur.Backend().Name)
	}
	f.onBitrate(bps)
	if s, ok := any(f.cur).(BitrateSetter); ok {
		return s.SetBitrate(bps)
	}
	return errBitrateUnsupported(f.cur.Backend().Name)
}
