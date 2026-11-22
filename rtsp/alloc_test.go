package rtsp

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/aep/gaal/rtsp/rtsptest"
)

func TestReceivePathDoesNotAllocate(t *testing.T) {
	h := newHarness(t, h264SDP(), 32, Config{})
	idr := fuA(idrNAL(6000), 1200)
	var pkts [][]byte
	var ts uint32
	seq := uint16(0)
	for f := 0; f < 8; f++ {
		if f == 0 {
			for i, p := range idr {
				pkts = append(pkts, rtpPacket(seq, ts, 96, i == len(idr)-1, p))
				seq++
			}
		} else {
			pkts = append(pkts, rtpPacket(seq, ts, 96, true, pNAL(300)))
			seq++
		}
		ts += 3000
	}
	step := func() {
		for _, p := range pkts {
			// Rewrite the header in place, as if each were a new packet.
			p[2], p[3] = byte(seq>>8), byte(seq)
			seq++
			u := binary.BigEndian.Uint32(p[4:]) + 24000
			binary.BigEndian.PutUint32(p[4:], u)
			h.tr.receive(p, h.now)
		}
		for {
			p := h.c.TryRead()
			if p == nil {
				break
			}
			p.Release()
		}
	}
	for i := 0; i < 20; i++ {
		step()
	}
	if n := testing.AllocsPerRun(200, step); n != 0 {
		t.Fatalf("%v allocations per GOP", n)
	}
}

func TestReorderPathDoesNotAllocate(t *testing.T) {
	h := newHarness(t, h264SDP(), 32, Config{})
	seq, ts := uint16(0), uint32(0)
	a, b := rtpPacket(0, 0, 96, true, pNAL(100)), rtpPacket(0, 0, 96, true, pNAL(100))
	set := func(p []byte, s uint16, t uint32) {
		p[2], p[3] = byte(s>>8), byte(s)
		p[4], p[5], p[6], p[7] = byte(t>>24), byte(t>>16), byte(t>>8), byte(t)
	}
	step := func() {
		set(a, seq+1, ts+3000)
		set(b, seq, ts)
		h.tr.receive(a, h.now)
		h.tr.receive(b, h.now)
		seq += 2
		ts += 6000
		for p := h.c.TryRead(); p != nil; p = h.c.TryRead() {
			p.Release()
		}
	}
	for i := 0; i < 50; i++ {
		step()
	}
	if n := testing.AllocsPerRun(500, step); n != 0 {
		t.Fatalf("%v allocations per reordered pair", n)
	}
}

func testEndToEndAllocs(t *testing.T, tr Transport) {
	pkt := rtpPacket(0, 0, 96, true, pNAL(1000))
	tick := make(chan struct{})
	srv := &rtsptest.Server{SDP: h264SDP()}
	srv.OnPlay = func(st *rtsptest.Stream) {
		seq, ts := uint16(0), uint32(0)
		for {
			select {
			case <-tick:
			case <-st.Done():
				return
			}
			pkt[2], pkt[3] = byte(seq>>8), byte(seq)
			pkt[4], pkt[5], pkt[6], pkt[7] = byte(ts>>24), byte(ts>>16), byte(ts>>8), byte(ts)
			seq++
			ts += 3000
			if st.WriteRTP(0, pkt) != nil {
				return
			}
		}
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{URL: srv.URL(), Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	step := func() {
		tick <- struct{}{}
		p, err := c.ReadTimeout(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		p.Release()
	}
	for i := 0; i < 500; i++ {
		step()
	}
	// A rare allocation in the runtime or the keepalive is tolerated.
	if n := testing.AllocsPerRun(2000, step); n > 0.05 {
		t.Fatalf("%v allocations per frame over %v", n, tr)
	}
}

func TestEndToEndAllocsTCP(t *testing.T) { testEndToEndAllocs(t, TransportTCP) }
func TestEndToEndAllocsUDP(t *testing.T) { testEndToEndAllocs(t, TransportUDP) }

func BenchmarkReceiveH264(b *testing.B) {
	h := newHarness(b, h264SDP(), 32, Config{})
	pkt := rtpPacket(0, 0, 96, true, pNAL(1200))
	b.ReportAllocs()
	b.SetBytes(int64(len(pkt)))
	seq, ts := uint16(0), uint32(0)
	for i := 0; i < b.N; i++ {
		pkt[2], pkt[3] = byte(seq>>8), byte(seq)
		pkt[4], pkt[5], pkt[6], pkt[7] = byte(ts>>24), byte(ts>>16), byte(ts>>8), byte(ts)
		seq++
		ts += 3000
		h.tr.receive(pkt, h.now)
		h.c.TryRead().Release()
	}
}
