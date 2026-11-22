package rtsp

import (
	"math/rand/v2"
	"testing"
	"time"
)

// feed sends one single-NAL picture per sequence number, in the given order.
func feedOrder(h *harness, base uint16, order []int) {
	for _, i := range order {
		seq := base + uint16(i)
		h.tr.receive(rtpPacket(seq, uint32(i+1)*3000, 96, true, pNAL(20)), h.now)
	}
}

func ptsOrder(ps []*Packet) []uint32 {
	var out []uint32
	for _, p := range ps {
		out = append(out, p.RTPTimestamp/3000)
	}
	return out
}

func TestReorderRestoresOrder(t *testing.T) {
	h := newHarness(t, h264SDP(), 16, Config{})
	feedOrder(h, 65530, []int{0, 2, 1, 3, 6, 5, 4, 7, 8, 9, 10}) // crosses the wrap
	got := ptsOrder(h.out())
	if len(got) != 11 {
		t.Fatalf("got %v", got)
	}
	for i, v := range got {
		if int(v) != i+1 {
			t.Fatalf("order %v", got)
		}
	}
	s := h.c.Stats().Tracks[0]
	if s.PacketsLost != 0 || s.PacketsReordered == 0 {
		t.Fatalf("%+v", s)
	}
}

func TestReorderDeclaresLossAfterDelay(t *testing.T) {
	h := newHarness(t, h264SDP(), 16, Config{})
	feedOrder(h, 100, []int{0, 2, 3})
	if got := h.out(); len(got) != 1 {
		t.Fatalf("released early: %v", ptsOrder(got))
	}
	h.tr.expire(h.now.Add(10 * time.Millisecond))
	if len(h.out()) != 0 {
		t.Fatal("gave up too soon")
	}
	h.tr.expire(h.now.Add(50 * time.Millisecond))
	out := h.out()
	if len(out) != 2 || !out[0].Discontinuity || out[1].Discontinuity {
		t.Fatalf("got %v", ptsOrder(out))
	}
	if h.c.Stats().Tracks[0].PacketsLost != 1 {
		t.Fatal("loss not counted")
	}
	// The straggler now counts as late and is ignored.
	feedOrder(h, 100, []int{1})
	if len(h.out()) != 0 || h.c.Stats().Tracks[0].PacketsLate != 1 {
		t.Fatal("late packet delivered")
	}
}

func TestReorderWindowOverflow(t *testing.T) {
	h := newHarness(t, h264SDP(), 8, Config{})
	order := []int{0}
	for i := 2; i < 30; i++ {
		order = append(order, i)
	}
	feedOrder(h, 0, order)
	out := h.out()
	if len(out) < 20 {
		t.Fatalf("window did not push packets out: %d", len(out))
	}
	if h.c.Stats().Tracks[0].PacketsLost != 1 {
		t.Fatalf("%+v", h.c.Stats().Tracks[0])
	}
	if h.tr.held >= 8 {
		t.Fatalf("held %d", h.tr.held)
	}
}

func TestDuplicatesIgnored(t *testing.T) {
	h := newHarness(t, h264SDP(), 8, Config{})
	feedOrder(h, 0, []int{0, 0, 2, 2, 1, 1})
	if got := ptsOrder(h.out()); len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	s := h.c.Stats().Tracks[0]
	if s.PacketsDuplicate != 1 || s.PacketsLate != 2 {
		t.Fatalf("%+v", s)
	}
}

func TestSenderRestartResyncs(t *testing.T) {
	h := newHarness(t, h264SDP(), 8, Config{})
	feedOrder(h, 30000, []int{0, 1, 2})
	h.out()
	for i := 0; i < maxLateRun+2; i++ {
		h.tr.receive(rtpPacket(uint16(20000+i), uint32(100+i)*3000, 96, true, pNAL(20)), h.now)
	}
	out := h.out()
	if len(out) < 2 || !out[0].Discontinuity {
		t.Fatalf("no resync: %d", len(out))
	}
}

func TestNewSSRCResyncs(t *testing.T) {
	h := newHarness(t, h264SDP(), 8, Config{})
	feedOrder(h, 10, []int{0, 1})
	h.out()
	p := rtpPacket(5000, 99000, 96, true, pNAL(20))
	p[8] ^= 0xFF
	h.tr.receive(p, h.now)
	out := h.out()
	if len(out) != 1 || !out[0].Discontinuity {
		t.Fatalf("got %d", len(out))
	}
}

func TestRandomLossAndReorderAccounting(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	h := newHarness(t, h264SDP(), 32, Config{QueueSize: 100000})
	const total = 20000
	var sent []int
	dropped := 0
	for i := 0; i < total; i++ {
		if rng.IntN(100) < 2 {
			dropped++
			continue
		}
		sent = append(sent, i)
	}
	for i := 0; i+1 < len(sent); i++ { // local swaps within a few positions
		if rng.IntN(100) < 5 {
			j := min(i+1+rng.IntN(4), len(sent)-1)
			sent[i], sent[j] = sent[j], sent[i]
		}
	}
	for _, i := range sent {
		h.now = h.now.Add(time.Millisecond)
		h.tr.receive(rtpPacket(uint16(i), uint32(i+1)*3000, 96, true, pNAL(20)), h.now)
		h.tr.expire(h.now)
	}
	h.tr.expire(h.now.Add(time.Second))
	got := ptsOrder(h.out())
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("out of order at %d: %d after %d", i, got[i], got[i-1])
		}
	}
	s := h.c.Stats().Tracks[0]
	if s.PacketsLost < uint64(dropped) || s.PacketsLost > uint64(dropped)+uint64(dropped)/4 {
		t.Fatalf("lost %d, really dropped %d", s.PacketsLost, dropped)
	}
	if want := total - int(s.PacketsLost); len(got) < want-int(s.PacketsLate) {
		t.Fatalf("delivered %d of %d", len(got), want)
	}
}
