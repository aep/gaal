package camera

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aep/gaal"
	"github.com/aep/gaal/rtsp"
)

// RTSPConfig configures OpenRTSP.
type RTSPConfig struct {
	// RTSP selects the stream. Its Select field is replaced: the first video
	// track is used.
	RTSP rtsp.Config
	// KeepDecoderDelay turns off gaal.VideoConfig.LowDelay, and rtsp.Config.LowDelay
	// with it, for a stream that does reorder pictures. A low-delay decoder
	// notices such a stream and restores the order, at the cost of a few pictures,
	// so this is only needed to keep every one of them.
	KeepDecoderDelay bool
	Library          *gaal.Library // default: gaal.New with default options
}

// RTSPTiming says when a frame's data arrived and when it was decoded.
type RTSPTiming struct {
	Arrival time.Time // last RTP packet of the access unit was read
	Sent    time.Time // the access unit was given to the decoder
	Decoded time.Time // the decoder returned the frame
	// Latency is Decoded minus Arrival: queueing plus decoding, on this machine.
	Latency time.Duration
}

// RTSPStats counts what the decoding side did.
type RTSPStats struct {
	Client        rtsp.Stats
	Sent          uint64 // packets given to the decoder
	Frames        uint64 // frames returned
	SkippedForKey uint64 // packets dropped while waiting for a keyframe
	DecodeErrors  uint64 // packets the decoder rejected as corrupt
	// Reorder says whether the stream broke the low-delay assumption.
	Reorder gaal.ReorderStats
}

// RTSPCamera decodes the first video track of an RTSP stream. It implements
// Camera. Like every Camera it is not safe for concurrent use, except Stats.
type RTSPCamera struct {
	client  *rtsp.Client
	track   rtsp.Track
	decoder gaal.VideoDecoder

	waitKey     bool
	outstanding int
	arrivals    [64]arrivalMark
	next        int
	stats       RTSPStats
}

type arrivalMark struct {
	pts     time.Duration
	arrival time.Time
	sent    time.Time
	used    bool
}

// OpenRTSP connects to the stream and opens a decoder for its video track.
func OpenRTSP(ctx context.Context, cfg RTSPConfig) (*RTSPCamera, error) {
	lib := cfg.Library
	if lib == nil {
		lib = gaal.New(gaal.Options{})
	}
	picked := false
	rc := cfg.RTSP
	if cfg.KeepDecoderDelay {
		rc.LowDelay = false
	}
	rc.Select = func(t rtsp.Track) bool {
		if picked || t.Media != rtsp.Video {
			return false
		}
		picked = true
		return true
	}
	client, err := rtsp.Dial(ctx, rc)
	if err != nil {
		return nil, err
	}
	track := client.Tracks()[0]
	vc := track.VideoConfig()
	vc.LowDelay = !cfg.KeepDecoderDelay
	dec, err := lib.NewVideoDecoder(vc)
	if err != nil {
		client.Close()
		return nil, err
	}
	return &RTSPCamera{client: client, track: track, decoder: dec, waitKey: true}, nil
}

// Name describes the stream and decoder.
func (c *RTSPCamera) Name() string {
	t := c.Track()
	return fmt.Sprintf("rtsp %s via %s", t, c.decoder.Backend().Name)
}

// Track returns the current description of the video track.
func (c *RTSPCamera) Track() rtsp.Track { return c.client.Tracks()[0] }

// Client returns the underlying RTSP client.
func (c *RTSPCamera) Client() *rtsp.Client { return c.client }

// Backend reports the decoder in use.
func (c *RTSPCamera) Backend() gaal.BackendInfo { return c.decoder.Backend() }

// Stats returns current counters. Call it from the goroutine using Next.
func (c *RTSPCamera) Stats() RTSPStats {
	s := c.stats
	s.Client = c.client.Stats()
	if r, ok := c.decoder.(gaal.ReorderReporter); ok {
		s.Reorder = r.ReorderStats()
	}
	return s
}

// Next returns the next decoded frame, which the caller must Release.
func (c *RTSPCamera) Next(timeout time.Duration) (*gaal.VideoFrame, error) {
	f, _, err := c.NextTimed(timeout)
	return f, err
}

// NextTimed is Next that also reports timing. After packet loss it skips to the
// next keyframe, so frames never show damage from missing references.
func (c *RTSPCamera) NextTimed(timeout time.Duration) (*gaal.VideoFrame, RTSPTiming, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := c.decoder.Receive()
		if err == nil {
			return c.deliver(f)
		}
		if !errors.Is(err, gaal.ErrAgain) && !errors.Is(err, gaal.ErrCorrupt) {
			return nil, RTSPTiming{}, err
		}
		p := c.client.TryRead()
		if p == nil {
			left := time.Until(deadline)
			if left <= 0 {
				return nil, RTSPTiming{}, fmt.Errorf("camera: timed out: %w", rtsp.ErrTimeout)
			}
			if c.outstanding > 0 {
				// A frame is in the decoder: wait for it briefly, then look for
				// more input, since the decoder may need that to finish.
				f, err := gaal.ReceiveWait(c.decoder, min(left, 2*time.Millisecond))
				if err == nil {
					return c.deliver(f)
				}
				if !errors.Is(err, gaal.ErrTimeout) && !errors.Is(err, gaal.ErrAgain) && !errors.Is(err, gaal.ErrCorrupt) {
					return nil, RTSPTiming{}, err
				}
				if p = c.client.TryRead(); p == nil {
					continue
				}
			} else if p, err = c.client.ReadTimeout(left); err != nil {
				if errors.Is(err, rtsp.ErrTimeout) {
					continue
				}
				return nil, RTSPTiming{}, err
			}
		}
		if err := c.feed(p); err != nil {
			return nil, RTSPTiming{}, err
		}
	}
}

func (c *RTSPCamera) feed(p *rtsp.Packet) error {
	defer p.Release()
	if p.Discontinuity {
		c.waitKey = true
	}
	if c.waitKey {
		if !p.Keyframe {
			c.stats.SkippedForKey++
			return nil
		}
		c.waitKey = false
	}
	c.arrivals[c.next] = arrivalMark{pts: p.PTS, arrival: p.Arrival, sent: time.Now(), used: true}
	c.next = (c.next + 1) % len(c.arrivals)
	err := c.decoder.Send(p.Packet)
	if errors.Is(err, gaal.ErrCorrupt) || errors.Is(err, gaal.ErrInvalid) {
		c.stats.DecodeErrors++
		c.waitKey = true
		return nil
	}
	if err != nil {
		return err
	}
	c.stats.Sent++
	c.outstanding++
	return nil
}

func (c *RTSPCamera) deliver(f *gaal.VideoFrame) (*gaal.VideoFrame, RTSPTiming, error) {
	now := time.Now()
	c.outstanding = max(c.outstanding-1, 0)
	c.stats.Frames++
	t := RTSPTiming{Decoded: now}
	for i := range c.arrivals {
		if m := &c.arrivals[i]; m.used && m.pts == f.PTS {
			t.Arrival, t.Sent, t.Latency = m.arrival, m.sent, now.Sub(m.arrival)
			m.used = false
			break
		}
	}
	return f, t, nil
}

// Close stops the stream, sending TEARDOWN, and closes the decoder.
func (c *RTSPCamera) Close() error {
	err := c.client.Close()
	if derr := c.decoder.Close(); err == nil && !errors.Is(derr, io.EOF) {
		err = derr
	}
	return err
}

var _ Camera = (*RTSPCamera)(nil)
