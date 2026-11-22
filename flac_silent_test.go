package gaal

import (
	"bytes"
	"slices"
	"testing"
)

func TestFLACUTF8(t *testing.T) {
	for _, v := range []uint64{0, 1, 0x7F, 0x80, 0x7FF, 0x800, 0xFFFF, 0x10000, 0x1FFFFF, 0x200000, 1<<31 - 1, 1 << 31, 1<<36 - 1} {
		b := flacPutUTF8(nil, v)
		got, n := flacUTF8(b)
		if got != v || n != len(b) {
			t.Errorf("%#x: encoded % x, read %#x in %d bytes", v, b, got, n)
		}
	}
}

// TestFLACSilentAfter replaces a frame of a real stream with a synthesized one
// and checks that libFLAC decodes the result to full length.
func TestFLACSilentAfter(t *testing.T) {
	for _, ch := range []int{1, 2, 6} {
		cfg := AudioConfig{Codec: FLAC, SampleRate: 48000, Channels: ch}
		l := New(Options{Backend: "libflac"})
		pcm := tonePCM(cfg.SampleRate, ch, 1)
		enc, err := l.NewAudioEncoder(cfg)
		if err != nil {
			t.Skipf("libflac: %v", err)
		}
		enc.Close()
		e := encodeAudio(t, l, cfg, pcm)
		var frames []Packet
		for _, p := range e.packets {
			if len(p.Data) >= 4 && string(p.Data[:4]) == "fLaC" {
				continue
			}
			frames = append(frames, p)
		}
		at := len(frames) / 2
		var si []byte
		if b := flacConfigBlocks(e.config); len(b) >= 4+4+34 {
			si = b[8:]
		}
		silent := flacSilentAfter(frames[at-1].Data, si)
		if !flacFrameValid(silent) {
			t.Fatalf("%d channels: synthesized frame is not valid: % x", ch, silent)
		}
		want, _ := flacUTF8(frames[at].Data[4:])
		if got, _ := flacUTF8(silent[4:]); got != want {
			t.Errorf("%d channels: frame number %d, want %d", ch, got, want)
		}
		dcfg := cfg
		dcfg.Extra = e.config
		clean, corrupt0, _ := decodeAudio(t, l, dcfg, e.packets, nil)
		out, corrupt, _ := decodeAudio(t, l, dcfg, e.packets, func(i int, p Packet) Packet {
			if &e.packets[i].Data[0] == &frames[at].Data[0] {
				p.Data = silent
			}
			return p
		})
		if corrupt0 != 0 || corrupt != 0 {
			t.Errorf("%d channels: corrupt packets %d, %d", ch, corrupt0, corrupt)
		}
		if len(out.pcm) != len(clean.pcm) {
			t.Errorf("%d channels: %d samples, want %d", ch, len(out.pcm), len(clean.pcm))
		}
	}
}

// TestFLACRenumber renumbers a real stream from 1000 and checks that libFLAC
// decodes it to the same samples, and that renumbering in place changes nothing.
func TestFLACRenumber(t *testing.T) {
	cfg := AudioConfig{Codec: FLAC, SampleRate: 44100, Channels: 2}
	l := New(Options{Backend: "libflac"})
	if enc, err := l.NewAudioEncoder(cfg); err != nil {
		t.Skipf("libflac: %v", err)
	} else {
		enc.Close()
	}
	e := encodeAudio(t, l, cfg, tonePCM(cfg.SampleRate, cfg.Channels, 1))
	num := uint64(1000)
	var renumbered []Packet
	for _, p := range e.packets {
		h, ok := parseFLACHeader(p.Data)
		if !ok {
			renumbered = append(renumbered, p)
			continue
		}
		if same := flacRenumber(nil, p.Data, h, h.num); !bytes.Equal(same, p.Data) {
			t.Fatalf("renumbering frame %d in place changed it", h.num)
		}
		q := p
		q.Data = flacRenumber(nil, p.Data, h, num)
		if g, ok := parseFLACHeader(q.Data); !ok || g.num != num {
			t.Fatalf("renumbered frame: parsed %v, number %d, want %d", ok, g.num, num)
		}
		num = h.next() - h.num + num
		renumbered = append(renumbered, q)
	}
	dcfg := cfg
	dcfg.Extra = e.config
	clean, _, _ := decodeAudio(t, l, dcfg, e.packets, nil)
	out, corrupt, _ := decodeAudio(t, l, dcfg, renumbered, nil)
	if corrupt != 0 || !slices.Equal(out.pcm, clean.pcm) {
		t.Errorf("renumbered stream: %d corrupt, %d samples, want %d identical", corrupt, len(out.pcm), len(clean.pcm))
	}
}

func TestFLACHeaderComplete(t *testing.T) {
	si := make([]byte, 34)
	block := func(last bool, typ byte, n int) []byte {
		b := []byte{typ, byte(n >> 16), byte(n >> 8), byte(n)}
		if last {
			b[0] |= 0x80
		}
		return append(b, make([]byte, n)...)
	}
	head := []byte("fLaC")
	withSI := append(append([]byte(nil), head...), block(false, 0, 34)[:4]...)
	withSI = append(withSI, si...)
	for _, tc := range []struct {
		b    []byte
		want bool
	}{
		{head, false},
		{withSI, false},
		{append(append([]byte(nil), withSI...), block(true, 4, 40)...), true},
		{append([]byte("fLaC"), block(true, 0, 34)...), true},
		{append([]byte("fLaC"), block(true, 4, 40)...), false}, // STREAMINFO must come first
	} {
		if got := flacHeaderComplete(tc.b); got != tc.want {
			t.Errorf("% x: %v, want %v", tc.b[:min(12, len(tc.b))], got, tc.want)
		}
	}
}
