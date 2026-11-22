package gaal

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
)

const lowDelayStep = 33 * time.Millisecond

func (c lowDelayClip) packets(t testing.TB) []Packet {
	t.Helper()
	out := make([]Packet, len(c.data))
	for i, s := range c.data {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = Packet{Data: b, PTS: time.Duration(c.pts[i]) * lowDelayStep, Keyframe: i == 0}
	}
	return out
}

// lowDelayCameraClip looks like a camera stream: no B-frames, but nothing in the
// SPS says so, and a level that allows the longest reordering.
func lowDelayCameraClip(t testing.TB) []Packet {
	pkts := lowDelayH264P.packets(t)
	for i := range pkts {
		pkts[i].Data = confNoReorderLimit(pkts[i].Data)
	}
	return pkts
}

func lowDelayOpen(t *testing.T, name string, c Codec, low bool) VideoDecoder {
	t.Helper()
	d, err := New(Options{Backend: name}).NewVideoDecoder(VideoConfig{Codec: c, LowDelay: low})
	if err != nil {
		confUnavailable(t, c, Decode, name, err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// lowDelayAllowed lists decoders that cannot return a picture before the next
// packet arrives, with how many they may hold.
var lowDelayAllowed = map[string]int{}

// confLowDelay checks that each packet produces its own picture at once.
func confLowDelay(t *testing.T, name string, c Codec, pkts []Packet) {
	t.Helper()
	d := lowDelayOpen(t, name, c, true)
	allowed := lowDelayAllowed[name]
	wait := confWait
	if allowed > 0 {
		wait = 20 * time.Millisecond
	}
	got := 0
	take := func(f *VideoFrame) {
		t.Helper()
		if got >= len(pkts) {
			t.Fatalf("more pictures than packets")
		}
		if want := pkts[got].PTS; f.PTS != want {
			t.Fatalf("picture %d has PTS %v, its packet %v", got, f.PTS, want)
		}
		got++
		f.Release()
	}
	for i, p := range pkts {
		if err := d.Send(p); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if f, err := ReceiveWait(d, wait); err == nil {
			take(f)
		} else if err != ErrTimeout {
			t.Fatalf("packet %d: %v", i, err)
		}
		for {
			f, err := d.Receive()
			if err != nil {
				break
			}
			take(f)
		}
		if held := i + 1 - got; held > allowed {
			t.Fatalf("decoder holds %d pictures after packet %d", held, i)
		}
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := ReceiveWait(d, confWait)
		if err != nil {
			break
		}
		take(f)
	}
	if got != len(pkts) {
		t.Errorf("decoded %d of %d pictures", got, len(pkts))
	}
	if s := d.(ReorderReporter).ReorderStats(); s != (ReorderStats{}) {
		t.Errorf("stream without reordering reported %+v", s)
	}
}

func TestLowDelayWithoutReorderLimit(t *testing.T) {
	for _, name := range confNames(H264, Decode) {
		t.Run(name, func(t *testing.T) { confLowDelay(t, name, H264, lowDelayCameraClip(t)) })
	}
}

// lowDelayDrain returns the timestamps of all remaining output.
func lowDelayDrain(t *testing.T, d VideoDecoder, out *[]time.Duration) {
	t.Helper()
	for {
		f, err := ReceiveWait(d, confWait)
		if err == io.EOF || err == ErrTimeout {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		*out = append(*out, f.PTS)
		f.Release()
	}
}

func TestDecoderOutputOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codec Codec
		clip  lowDelayClip
	}{{"h264", H264, lowDelayH264B}, {"h265", H265, lowDelayH265B}} {
		for _, name := range confNames(tc.codec, Decode) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				pkts := tc.clip.packets(t)
				t.Run("normal", func(t *testing.T) {
					d := lowDelayOpen(t, name, tc.codec, false)
					var got []time.Duration
					for i, p := range pkts {
						if err := d.Send(p); err != nil {
							t.Fatalf("send %d: %v", i, err)
						}
						for {
							f, err := d.Receive()
							if err != nil {
								break
							}
							got = append(got, f.PTS)
							f.Release()
						}
					}
					if err := d.Flush(); err != nil {
						t.Fatal(err)
					}
					lowDelayDrain(t, d, &got)
					if len(got) != len(pkts) {
						t.Errorf("decoded %d of %d pictures", len(got), len(pkts))
					}
					if !slices.IsSorted(got) {
						t.Errorf("output order %v", got)
					}
				})
				t.Run("low-delay", func(t *testing.T) {
					d := lowDelayOpen(t, name, tc.codec, true)
					var got []time.Duration
					for i, p := range pkts {
						if err := d.Send(p); err != nil {
							t.Fatalf("send %d: %v", i, err)
						}
						for {
							f, err := d.Receive()
							if err != nil {
								break
							}
							got = append(got, f.PTS)
							f.Release()
						}
					}
					if err := d.Flush(); err != nil {
						t.Fatal(err)
					}
					lowDelayDrain(t, d, &got)
					if !slices.IsSorted(got) {
						t.Errorf("output order %v", got)
					}
					s := d.(ReorderReporter).ReorderStats()
					if s.Reordered == 0 || s.Depth == 0 {
						t.Errorf("reordering was not detected: %+v", s)
					}
					if len(got)+int(s.Dropped) != len(pkts) {
						t.Errorf("%d pictures returned and %d dropped, of %d", len(got), s.Dropped, len(pkts))
					}
				})
			})
		}
	}
}

// lowDelayFake is a decoder that returns pictures in decoding order.
type lowDelayFake struct {
	out     fifo[*VideoFrame]
	flushed bool
}

func (d *lowDelayFake) Send(p Packet) error {
	f := NewVideoFrame(2, 2, NV12)
	f.PTS = p.PTS
	d.out.push(f)
	return nil
}

func (d *lowDelayFake) Receive() (*VideoFrame, error) {
	if f, ok := d.out.pop(); ok {
		return f, nil
	}
	return nil, endOfQueue(d.flushed)
}

func (d *lowDelayFake) Flush() error         { d.flushed = true; return nil }
func (d *lowDelayFake) Close() error         { return nil }
func (d *lowDelayFake) Backend() BackendInfo { return BackendInfo{Name: "fake"} }

// The guard works from the bitstream, so it needs no codec on this machine.
func TestReorderGuard(t *testing.T) {
	run := func(t *testing.T, c Codec, pkts []Packet) ([]time.Duration, ReorderStats) {
		d := &colorDecoder{VideoDecoder: &lowDelayFake{}, tracker: newColorTracker(c), guard: newReorderGuard(c)}
		defer d.Close()
		var got []time.Duration
		for _, p := range pkts {
			if err := d.Send(p); err != nil {
				t.Fatal(err)
			}
			for {
				f, err := d.Receive()
				if errors.Is(err, ErrAgain) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, f.PTS)
				f.Release()
			}
		}
		d.Flush()
		for {
			f, err := d.Receive()
			if err == io.EOF {
				return got, d.ReorderStats()
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, f.PTS)
			f.Release()
		}
	}
	t.Run("no-reordering", func(t *testing.T) {
		pkts := lowDelayCameraClip(t)
		got, s := run(t, H264, pkts)
		if s != (ReorderStats{}) || len(got) != len(pkts) {
			t.Errorf("%+v, %d of %d pictures", s, len(got), len(pkts))
		}
	})
	for _, tc := range []struct {
		name  string
		codec Codec
		clip  lowDelayClip
		depth int
	}{{"h264", H264, lowDelayH264B, 2}, {"h265", H265, lowDelayH265B, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			pkts := tc.clip.packets(t)
			got, s := run(t, tc.codec, pkts)
			if !slices.IsSorted(got) {
				t.Errorf("output order %v", got)
			}
			if s.Depth < tc.depth || s.Reordered == 0 || s.Dropped == 0 {
				t.Errorf("stats %+v", s)
			}
			if len(got)+int(s.Dropped) != len(pkts) {
				t.Errorf("%d returned, %d dropped, of %d", len(got), s.Dropped, len(pkts))
			}
			// Only the pictures before the first detection may be lost.
			if int(s.Dropped) > 2*s.Depth {
				t.Errorf("dropped %d pictures for depth %d", s.Dropped, s.Depth)
			}
		})
	}
	t.Run("states-no-reorder", func(t *testing.T) {
		sps := func(au []byte) h264Seq {
			for pos := 0; ; {
				i := bytes.Index(au[pos:], startCode3)
				if i < 0 {
					t.Fatal("no SPS")
				}
				start := pos + i + 3
				end := len(au)
				if j := bytes.Index(au[start:], startCode3); j >= 0 {
					end = start + j
				}
				if au[start]&0x1F == 7 {
					g := newReorderGuard(H264)
					return parseH264Seq(g.rbsp(au[start+1:end], 512))
				}
				pos = end
			}
		}
		for _, au := range [][]byte{lowDelayH264P.packets(t)[0].Data, lowDelayCameraClip(t)[0].Data} {
			g := newReorderGuard(H264)
			out := g.statesNoReorder(au)
			before, after := sps(au), sps(out)
			if !after.restricted || after.maxReorder != 0 || after.maxBuffering < 1 {
				t.Errorf("rewritten SPS: %+v", after)
			}
			if after.mbW != before.mbW || after.mbH != before.mbH || after.pocType != before.pocType ||
				after.lsbLen != before.lsbLen || after.numRef != before.numRef || after.profile != before.profile {
				t.Errorf("SPS changed from %+v to %+v", before, after)
			}
			if StreamRange(H264, out) != StreamRange(H264, au) {
				t.Error("color range changed")
			}
			slice := au[bytes.LastIndex(au, startCode3):]
			if !bytes.HasSuffix(out, slice) {
				t.Error("picture data changed")
			}
			if again := g.statesNoReorder(out); !bytes.Equal(again, out) {
				t.Error("a stream that already states it was rewritten")
			}
		}
	})
	t.Run("damaged-input", func(t *testing.T) {
		for _, c := range []Codec{H264, H265} {
			g := newReorderGuard(c)
			for n := 0; n < 40; n++ {
				for _, b := range [][]byte{
					{0, 0, 1}, {0, 0, 1, 0, 0, 1}, {0, 0, 1, 0x67}, {0, 0, 1, 0x68, 0xff}, {0, 0, 1, 0x41}, {0, 0, 1, 0x65, 0x80},
					{0, 0, 1, 0x42, 1}, {0, 0, 1, 0x44, 1, 0}, {0, 0, 1, 0x26, 1, 0xff}, bytes.Repeat([]byte{0, 0, 1, 0x67, 0, 0}, 9),
				} {
					g.observe(Packet{Data: b})
					g.statesNoReorder(b)
				}
			}
		}
	})
}

// A low-delay decoder must not allocate per picture either.
func TestLowDelayAllocations(t *testing.T) {
	pkts := lowDelayCameraClip(t)
	for _, name := range confNames(H264, Decode) {
		t.Run(name, func(t *testing.T) {
			d := lowDelayOpen(t, name, H264, true)
			fb := hwFallbacks
			run := func() {
				for _, p := range pkts {
					if err := d.Send(p); err != nil {
						if hwFallback(t, err) {
							continue
						}
						t.Fatal(err)
					}
					wait := confWait
					if lowDelayAllowed[name] > 0 {
						wait = 5 * time.Millisecond
					}
					f, err := ReceiveWait(d, wait)
					// After a fallback, packets before the next keyframe give no picture.
					if (err == ErrTimeout && (wait != confWait || hwFallbacks != fb)) || hwFallback(t, err) {
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					f.Release()
				}
			}
			for i := 0; i < 3; i++ {
				run()
			}
			if got := allocsPerRun(20, run); got > 0 {
				t.Errorf("%.2f allocations per run", got)
			}
		})
	}
}

// confNoReorderLimit returns a copy of an H.264 access unit whose SPS claims a
// high level and no bitstream restriction, as many cameras send. A decoder must
// then assume that any picture may be reordered, unless told otherwise.
func confNoReorderLimit(au []byte) []byte {
	var out []byte
	for pos := 0; ; {
		i := bytes.Index(au[pos:], startCode3)
		if i < 0 {
			return append(out, au[pos:]...)
		}
		start := pos + i + 3
		end := len(au)
		if j := bytes.Index(au[start:], startCode3); j >= 0 {
			end = start + j
		}
		out = append(out, au[pos:start]...)
		nal := au[start:end]
		trail := len(nal)
		for trail > 0 && nal[trail-1] == 0 {
			trail--
		}
		if trail > 0 && nal[0]&31 == 7 {
			out = append(out, confRewriteSPS(nal[:trail])...)
			out = append(out, nal[trail:]...)
		} else {
			out = append(out, nal...)
		}
		pos = end
	}
}

func confRewriteSPS(nal []byte) []byte {
	var rbsp []byte
	for i, zeros := 1, 0; i < len(nal); i++ {
		if zeros >= 2 && nal[i] == 3 {
			zeros = 0
			continue
		}
		rbsp = append(rbsp, nal[i])
		if nal[i] == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	rbsp[2] = 51 // level_idc
	r := bitReader{b: rbsp}
	profile := r.u(8)
	r.skip(16)
	r.ue()
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := r.ue()
		if chroma == 3 {
			r.skip(1)
		}
		r.ue()
		r.ue()
		r.skip(1)
		if r.bit() == 1 {
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.bit() == 1 {
					if i < 6 {
						r.h264ScalingList(16)
					} else {
						r.h264ScalingList(64)
					}
				}
			}
		}
	}
	r.ue()
	switch r.ue() {
	case 0:
		r.ue()
	case 1:
		r.skip(1)
		r.se()
		r.se()
		for n := r.ue(); n > 0; n-- {
			r.se()
		}
	}
	r.ue()
	r.skip(1)
	r.ue()
	r.ue()
	if r.bit() == 0 {
		r.skip(1)
	}
	r.skip(1)
	if r.bit() == 1 {
		r.ue()
		r.ue()
		r.ue()
		r.ue()
	}
	end := r.pos // vui_parameters_present_flag
	vui := r.bit() == 1
	if vui {
		if r.bit() == 1 && r.u(8) == 255 {
			r.skip(32)
		}
		if r.bit() == 1 {
			r.skip(1)
		}
		if r.bit() == 1 {
			r.skip(4)
			if r.bit() == 1 {
				r.skip(24)
			}
		}
		if r.bit() == 1 {
			r.ue()
			r.ue()
		}
		if r.bit() == 1 {
			r.skip(65)
		}
		hrd := func() {
			n := r.ue()
			r.skip(8)
			for i := uint32(0); i <= n; i++ {
				r.ue()
				r.ue()
				r.skip(1)
			}
			r.skip(20)
		}
		nal, vcl := r.bit() == 1, false
		if nal {
			hrd()
		}
		if vcl = r.bit() == 1; vcl {
			hrd()
		}
		if nal || vcl {
			r.skip(1)
		}
		r.skip(1)
		end = r.pos // bitstream_restriction_flag
	}
	var w []byte
	n := 0
	put := func(v uint32) {
		if n%8 == 0 {
			w = append(w, 0)
		}
		w[len(w)-1] |= byte(v&1) << (7 - n%8)
		n++
	}
	c := bitReader{b: rbsp}
	for c.pos < end {
		put(c.bit())
	}
	put(0) // no VUI, or no restriction
	put(1) // rbsp_stop_one_bit
	out := []byte{nal[0]}
	zeros := 0
	for _, v := range w {
		if zeros >= 2 && v <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
