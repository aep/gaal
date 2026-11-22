//go:build linux && !android && (amd64 || arm64)

package gaal

import "unsafe"

// vaRate holds the rate-control parameters an encoder resubmits when its
// bitrate changes. Drivers apply a rate-control buffer on any picture.
type vaRate struct {
	rc, hrd []byte
	dirty   bool
}

// set rewrites the parameters for bps. seqBps is the sequence parameter's
// bits_per_second field, which takes effect at the next keyframe.
func (r *vaRate) set(seq []byte, seqBps, bps, fps int) {
	vbv := uint32(bps / fps)
	le32(seq, seqBps, uint32(bps))
	le32(r.rc[vaoEMData:], vaoERBps, uint32(bps))
	le32(r.hrd[vaoEMData:], vaoEHFull, vbv)
	le32(r.hrd[vaoEMData:], vaoEHSize, vbv)
	r.dirty = true
}

// queue submits changed parameters with a picture that is not a keyframe;
// keyframes submit them anyway.
func (r *vaRate) queue(s *vaSession, idr bool) error {
	if !r.dirty {
		return nil
	}
	r.dirty = false
	if idr {
		return nil
	}
	for _, m := range [...][]byte{r.rc, r.hrd} {
		if err := s.addBuffer(vaBufEncMisc, unsafe.Pointer(&m[0]), len(m)); err != nil {
			return err
		}
	}
	return nil
}

// SetBitrate implements BitrateSetter.
func (e *vaH264Encoder) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if e.closed {
		return ErrClosed
	}
	e.cfg.Bitrate = bps
	e.rate.rc, e.rate.hrd = e.rc, e.hrd
	e.rate.set(e.seq, vaoESBps, bps, e.cfg.FPS)
	return nil
}

// SetBitrate implements BitrateSetter.
func (e *vaH265Encoder) SetBitrate(bps int) error {
	if err := checkBitrate(bps); err != nil {
		return err
	}
	if e.closed {
		return ErrClosed
	}
	e.cfg.Bitrate = bps
	e.rate.rc, e.rate.hrd = e.rc, e.hrd
	e.rate.set(e.seq, vaoHEBps, bps, e.cfg.FPS)
	return nil
}
