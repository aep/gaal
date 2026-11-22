package gaal

import (
	"bytes"
	"errors"
	"testing"
)

// gapStream writes H.264 parameter sets and slice headers, enough for
// frameNumGuard.
type gapStream struct {
	frameBits      int
	gapsAllowed    bool
	separatePlanes bool
}

func (w *spsWriter) u(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bit(v >> i)
	}
}

func gapNAL(header byte, w *spsWriter) []byte {
	w.bit(1) // rbsp_stop_one_bit
	return appendNAL([]byte{0, 0, 0, 1}, header, w.b)
}

func (s gapStream) sps(id uint32) []byte {
	var w spsWriter
	profile := uint32(66)
	if s.separatePlanes {
		profile = 244
	}
	w.u(profile, 8)
	w.u(0, 8)
	w.u(30, 8)
	w.ue(id)
	if s.separatePlanes {
		w.ue(3) // chroma_format_idc
		w.bit(1)
		w.ue(0)
		w.ue(0)
		w.bit(0)
		w.bit(0)
	}
	w.ue(uint32(s.frameBits - 4))
	w.ue(2) // pic_order_cnt_type
	w.ue(1) // max_num_ref_frames
	if s.gapsAllowed {
		w.bit(1)
	} else {
		w.bit(0)
	}
	w.ue(0) // one macroblock wide
	w.ue(0) // and high
	w.bit(1)
	w.bit(1)
	w.bit(0)
	w.bit(0)
	return gapNAL(0x67, &w)
}

func gapPPS(id, sps uint32) []byte {
	var w spsWriter
	w.ue(id)
	w.ue(sps)
	w.u(0, 7)
	return gapNAL(0x68, &w)
}

// slice returns a slice of a picture. idr implies a reference picture.
func (s gapStream) slice(pps, frameNum uint32, idr, ref bool) []byte {
	var w spsWriter
	w.ue(0) // first_mb_in_slice
	header := byte(0x01)
	switch {
	case idr:
		header = 0x65
		w.ue(7)
	case ref:
		header = 0x41
		w.ue(5)
	default:
		w.ue(5)
	}
	w.ue(pps)
	if s.separatePlanes {
		w.u(0, 2)
	}
	w.u(frameNum, s.frameBits)
	if idr {
		w.ue(0)
	}
	w.ue(0)
	return gapNAL(header, &w)
}

// gapPicture describes one picture in decoding order.
type gapPicture struct {
	frameNum uint32
	idr, ref bool
	lost     bool // not sent
	accepted bool // wanted result
	pps      uint32
}

func TestFrameNumGuard(t *testing.T) {
	plain := gapStream{frameBits: 4}
	params := func(s gapStream) []byte { return append(s.sps(0), gapPPS(0, 0)...) }
	run := func(t *testing.T, s gapStream, pics []gapPicture) {
		t.Helper()
		var g frameNumGuard
		for i, p := range pics {
			if p.lost {
				continue
			}
			au := s.slice(p.pps, p.frameNum, p.idr, p.ref || p.idr)
			if p.idr {
				au = append(params(s), au...)
			}
			if got := g.accept(au); got != p.accepted {
				t.Fatalf("picture %d (frame_num %d): accepted %v, want %v", i, p.frameNum, got, p.accepted)
			}
		}
	}
	idr := gapPicture{idr: true, accepted: true}
	ref := func(n uint32, ok bool) gapPicture { return gapPicture{frameNum: n, ref: true, accepted: ok} }
	nonRef := func(n uint32, ok bool) gapPicture { return gapPicture{frameNum: n, accepted: ok} }
	lost := func(n uint32) gapPicture { return gapPicture{frameNum: n, ref: true, lost: true} }

	t.Run("loss", func(t *testing.T) {
		run(t, plain, []gapPicture{idr, ref(1, true), ref(2, true), lost(3), ref(4, false), ref(5, false),
			idr, ref(1, true), ref(2, true)})
	})
	t.Run("non-reference", func(t *testing.T) {
		// Non-reference pictures do not count frame_num up, and losing one
		// harms no other picture.
		run(t, plain, []gapPicture{idr, nonRef(1, true), nonRef(1, true), ref(1, true), {frameNum: 2, lost: true},
			ref(2, true), nonRef(3, true), ref(3, true)})
	})
	t.Run("wrap", func(t *testing.T) {
		pics := []gapPicture{idr}
		for n := 1; n < 40; n++ {
			pics = append(pics, ref(uint32(n%16), true))
		}
		pics = append(pics, lost(8), ref(9, false))
		run(t, plain, pics)
	})
	t.Run("mid-stream", func(t *testing.T) {
		run(t, plain, []gapPicture{ref(5, false), ref(6, false), idr, ref(1, true)})
	})
	t.Run("gaps allowed", func(t *testing.T) {
		run(t, gapStream{frameBits: 4, gapsAllowed: true}, []gapPicture{idr, ref(1, true), lost(2), ref(3, true), ref(7, true)})
	})
	t.Run("separate planes", func(t *testing.T) {
		s := gapStream{frameBits: 6, separatePlanes: true}
		run(t, s, []gapPicture{idr, ref(1, true), ref(2, true), lost(3), ref(4, false)})
	})

	t.Run("parameter sets", func(t *testing.T) {
		var g frameNumGuard
		if !g.accept(params(plain)) {
			t.Fatal("parameter sets before an IDR picture refused")
		}
		if g.accept(plain.slice(0, 3, false, true)) {
			t.Fatal("picture before an IDR picture accepted")
		}
		if !g.accept(plain.slice(0, 0, true, true)) {
			t.Fatal("IDR picture after separate parameter sets refused")
		}
		// A second SPS with longer frame_num, used through its own PPS.
		long := gapStream{frameBits: 9}
		if !g.accept(append(long.sps(1), gapPPS(1, 1)...)) {
			t.Fatal("parameter sets refused")
		}
		for n := uint32(1); n < 300; n++ {
			if !g.accept(long.slice(1, n, false, true)) {
				t.Fatalf("frame_num %d of a 9-bit SPS refused", n)
			}
		}
		if g.accept(long.slice(1, 302, false, true)) {
			t.Fatal("gap accepted")
		}
	})
}

// A recorded stream with one lost picture.
func TestFrameNumGuardRecorded(t *testing.T) {
	pkts := lowDelayH264P.packets(t)
	var g frameNumGuard
	for i, p := range pkts {
		if i == 9 {
			continue
		}
		if got, want := g.accept(p.Data), i < 9; got != want {
			t.Fatalf("packet %d: accepted %v, want %v", i, got, want)
		}
	}
}

// gapCounter is a decoder that counts what reaches it.
type gapCounter struct {
	lowDelayFake
	sent int
}

func (d *gapCounter) Send(p Packet) error { d.sent++; return d.lowDelayFake.Send(p) }

// The decoder wrapper keeps pictures after a loss from every backend.
func TestFrameNumGuardDecoder(t *testing.T) {
	pkts := lowDelayH264P.packets(t)
	fake := &gapCounter{}
	d := &colorDecoder{VideoDecoder: fake, tracker: newColorTracker(H264), gaps: &frameNumGuard{}}
	defer d.Close()
	for i, p := range pkts {
		if i == 5 {
			continue
		}
		err := d.Send(p)
		if i < 5 && err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if i > 5 && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("packet %d after a loss: %v, want ErrCorrupt", i, err)
		}
	}
	if fake.sent != 5 {
		t.Fatalf("backend got %d packets, want 5", fake.sent)
	}
	if err := d.Send(pkts[0]); err != nil {
		t.Fatalf("IDR picture: %v", err)
	}
	if testing.AllocsPerRun(50, func() { d.gaps.accept(pkts[1].Data) }) > 0 {
		t.Error("the guard allocates")
	}
}

// Relabelling an SPS as baseline keeps every other field.
func TestBaselineSPS(t *testing.T) {
	sps := func(au []byte) []byte {
		i := bytes.Index(au, []byte{0, 0, 1, 0x67})
		j := bytes.Index(au[i+3:], startCode3)
		nal := au[i+3 : i+3+j]
		for nal[len(nal)-1] == 0 {
			nal = nal[:len(nal)-1]
		}
		return nal
	}
	for _, au := range [][]byte{lowDelayH264P.packets(t)[0].Data, lowDelayCameraClip(t)[0].Data} {
		nal := sps(au)
		rbsp := appendRBSP(nil, nal[1:], len(nal))
		out, ok := appendBaselineSPS(nil, nal, rbsp)
		if !ok {
			t.Fatal("SPS not relabelled")
		}
		was, now := parseH264Seq(rbsp), parseH264Seq(appendRBSP(nil, out[1:], len(out)))
		if was.profile == 66 || now.profile != 66 || !now.valid {
			t.Fatalf("profile %d relabelled as %d", was.profile, now.profile)
		}
		now.profile, now.vuiPos, now.restrictPos = was.profile, was.vuiPos, was.restrictPos
		if now != was {
			t.Fatalf("relabelled SPS reads\n%+v, was\n%+v", now, was)
		}
		if _, ok := appendBaselineSPS(nil, out, appendRBSP(nil, out[1:], len(out))); ok {
			t.Fatal("baseline SPS relabelled again")
		}
	}
}
