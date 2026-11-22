package camera

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/aep/gaal"
	"github.com/aep/gaal/rtsp"
)

// RTSPReport summarizes a measured run of ProbeRTSP.
type RTSPReport struct {
	Track     rtsp.Track
	Backend   string
	Hardware  bool
	Transport rtsp.Transport
	Duration  time.Duration

	Frames        int
	Width, Height int
	Format        gaal.PixelFormat
	FPS           float64 // frames per second of wall time
	StreamFPS     float64 // frames per second of presentation time

	// ArrivalJitter is the standard deviation of frame arrival against the
	// spacing the timestamps promise.
	ArrivalJitter time.Duration
	// Latency measures from the arrival of a frame's last packet to its decoded
	// frame being available.
	LatencyAvg, LatencyP50, LatencyP95, LatencyMax time.Duration

	// LumaMean and LumaStdDev average per-frame statistics of sampled pixels.
	LumaMean, LumaStdDev float64
	// VaryingFrames counts frames whose luma is not a constant color, and
	// ChangingFrames those whose sampled luma differs from the previous frame.
	VaryingFrames, ChangingFrames int

	Stats RTSPStats
}

func (r *RTSPReport) String() string {
	var b strings.Builder
	hw := "software"
	if r.Hardware {
		hw = "hardware"
	}
	fmt.Fprintf(&b, "stream:   %s, transport %s, decoder %s (%s)\n", r.Track, r.Transport, r.Backend, hw)
	fmt.Fprintf(&b, "frames:   %d at %dx%d in %v: %.2f fps (stream %.2f fps)\n", r.Frames, r.Width, r.Height, r.Duration.Round(time.Millisecond), r.FPS, r.StreamFPS)
	fmt.Fprintf(&b, "latency:  arrival to decoded avg %v, p50 %v, p95 %v, max %v\n", rnd(r.LatencyAvg), rnd(r.LatencyP50), rnd(r.LatencyP95), rnd(r.LatencyMax))
	fmt.Fprintf(&b, "jitter:   frame arrival %v\n", rnd(r.ArrivalJitter))
	fmt.Fprintf(&b, "luma:     mean %.1f, stddev %.1f, %d frames varied, %d changed from the previous\n", r.LumaMean, r.LumaStdDev, r.VaryingFrames, r.ChangingFrames)
	for _, t := range r.Stats.Client.Tracks {
		fmt.Fprintf(&b, "track %d:  %s packets %d, lost %d, late %d, reordered %d, duplicate %d, units %d, dropped %d, queue drops %d, %.2f Mbit/s, RFC 3550 jitter %v, unit jitter %v\n",
			t.Track, t.Codec, t.PacketsReceived, t.PacketsLost, t.PacketsLate, t.PacketsReordered, t.PacketsDuplicate, t.Units, t.UnitsDropped, t.QueueDrops,
			float64(t.BytesReceived)*8/1e6/max(r.Duration.Seconds(), 1e-9), rnd(t.Jitter), rnd(t.UnitJitter))
	}
	fmt.Fprintf(&b, "decoder:  sent %d, frames %d, skipped waiting for keyframe %d, errors %d\n", r.Stats.Sent, r.Stats.Frames, r.Stats.SkippedForKey, r.Stats.DecodeErrors)
	return b.String()
}

func rnd(d time.Duration) time.Duration { return d.Round(10 * time.Microsecond) }

const lumaGrid = 32

// ProbeRTSP decodes a stream for the given time and measures it. It looks at
// pixels only to compute statistics, and keeps none of them.
func ProbeRTSP(ctx context.Context, cfg RTSPConfig, d time.Duration) (*RTSPReport, error) {
	cam, err := OpenRTSP(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer cam.Close()
	r := &RTSPReport{Backend: cam.Backend().Name}
	var latencies []time.Duration
	var devSum, devSq, lumaSum, stdSum float64
	var devN int
	var firstPTS, lastPTS, prevPTS time.Duration
	var prevArrival, start time.Time
	var prev, cur [lumaGrid * lumaGrid]byte
	var runErr error
	for {
		if err := ctx.Err(); err != nil {
			runErr = err
			break
		}
		if !start.IsZero() && time.Since(start) >= d {
			break
		}
		f, tm, err := cam.NextTimed(10 * time.Second)
		if err != nil {
			runErr = err
			break
		}
		now := tm.Decoded
		if start.IsZero() {
			start = now
			firstPTS = f.PTS
			r.Width, r.Height, r.Format = f.Width, f.Height, f.Format
		}
		if f.Width != r.Width || f.Height != r.Height {
			r.Width, r.Height = f.Width, f.Height
		}
		r.Frames++
		lastPTS = max(lastPTS, f.PTS)
		if !tm.Arrival.IsZero() {
			latencies = append(latencies, tm.Latency)
			if !prevArrival.IsZero() && f.PTS > prevPTS {
				dev := tm.Arrival.Sub(prevArrival).Seconds() - (f.PTS - prevPTS).Seconds()
				devSum += dev
				devSq += dev * dev
				devN++
			}
			if f.PTS >= prevPTS {
				prevArrival, prevPTS = tm.Arrival, f.PTS
			}
		}
		mean, std := sampleLuma(f, &cur)
		lumaSum += mean
		stdSum += std
		if std > 1 {
			r.VaryingFrames++
		}
		if r.Frames > 1 && cur != prev {
			r.ChangingFrames++
		}
		prev = cur
		f.Release()
	}
	r.Transport = cam.Client().Stats().Transport
	r.Track = cam.Track()
	r.Hardware = cam.Backend().Hardware
	r.Backend = cam.Backend().Name
	r.Stats = cam.Stats()
	if r.Frames == 0 {
		if runErr == nil {
			runErr = errors.New("camera: no frames decoded")
		}
		return r, runErr
	}
	r.Duration = time.Since(start)
	r.FPS = float64(r.Frames) / r.Duration.Seconds()
	if span := (lastPTS - firstPTS).Seconds(); span > 0 {
		r.StreamFPS = float64(r.Frames-1) / span
	}
	r.LumaMean = lumaSum / float64(r.Frames)
	r.LumaStdDev = stdSum / float64(r.Frames)
	if devN > 1 {
		mean := devSum / float64(devN)
		r.ArrivalJitter = time.Duration(math.Sqrt(max(devSq/float64(devN)-mean*mean, 0)) * 1e9)
	}
	if n := len(latencies); n > 0 {
		slices.Sort(latencies)
		var total time.Duration
		for _, l := range latencies {
			total += l
		}
		r.LatencyAvg, r.LatencyMax = total/time.Duration(n), latencies[n-1]
		r.LatencyP50, r.LatencyP95 = latencies[n/2], latencies[n*95/100]
	}
	if runErr != nil && !errors.Is(runErr, context.DeadlineExceeded) && !errors.Is(runErr, context.Canceled) {
		return r, runErr
	}
	return r, nil
}

// sampleLuma fills grid with luma samples and returns their mean and deviation.
func sampleLuma(f *gaal.VideoFrame, grid *[lumaGrid * lumaGrid]byte) (mean, std float64) {
	var sum, sq float64
	for y := 0; y < lumaGrid; y++ {
		row := f.Y[(y*2+1)*f.Height/(2*lumaGrid)*f.YStride:]
		for x := 0; x < lumaGrid; x++ {
			v := row[(x*2+1)*f.Width/(2*lumaGrid)]
			grid[y*lumaGrid+x] = v
			sum += float64(v)
			sq += float64(v) * float64(v)
		}
	}
	n := float64(lumaGrid * lumaGrid)
	mean = sum / n
	return mean, math.Sqrt(max(sq/n-mean*mean, 0))
}
