package rtsp

import (
	"encoding/binary"
	"time"
)

type rtpHeader struct {
	marker bool
	pt     uint8
	seq    uint16
	ts     uint32
	ssrc   uint32
}

// parseRTP validates an RTP packet and returns its header and payload.
func parseRTP(b []byte) (h rtpHeader, payload []byte, ok bool) {
	if len(b) < 12 || b[0]>>6 != 2 {
		return h, nil, false
	}
	off := 12 + int(b[0]&0x0F)*4
	if off > len(b) {
		return h, nil, false
	}
	if b[0]&0x10 != 0 {
		if off+4 > len(b) {
			return h, nil, false
		}
		off += 4 + int(binary.BigEndian.Uint16(b[off+2:]))*4
		if off > len(b) {
			return h, nil, false
		}
	}
	end := len(b)
	if b[0]&0x20 != 0 {
		pad := int(b[end-1])
		if pad == 0 || pad > end-off {
			return h, nil, false
		}
		end -= pad
	}
	h.marker = b[1]&0x80 != 0
	h.pt = b[1] & 0x7F
	h.seq = binary.BigEndian.Uint16(b[2:])
	h.ts = binary.BigEndian.Uint32(b[4:])
	h.ssrc = binary.BigEndian.Uint32(b[8:])
	return h, b[off:end], true
}

const ntpEpochOffset = 2208988800

func ntpToTime(v uint64) time.Time {
	sec := int64(v>>32) - ntpEpochOffset
	nsec := int64((v & 0xFFFFFFFF) * 1_000_000_000 >> 32)
	return time.Unix(sec, nsec)
}

// rtcpEvents is what a compound RTCP packet says.
type rtcpEvents struct {
	hasSR   bool
	srSSRC  uint32
	srNTP   uint64
	srRTP   uint32
	bye     bool
	packets int
}

func parseRTCP(b []byte) (ev rtcpEvents, ok bool) {
	for len(b) >= 4 {
		if b[0]>>6 != 2 {
			return ev, false
		}
		n := (int(binary.BigEndian.Uint16(b[2:])) + 1) * 4
		if n > len(b) {
			return ev, false
		}
		ev.packets++
		switch b[1] {
		case 200:
			if n >= 28 {
				ev.hasSR = true
				ev.srSSRC = binary.BigEndian.Uint32(b[4:])
				ev.srNTP = binary.BigEndian.Uint64(b[8:])
				ev.srRTP = binary.BigEndian.Uint32(b[16:])
			}
		case 203:
			ev.bye = true
		}
		b = b[n:]
	}
	return ev, ev.packets > 0 && len(b) == 0
}

type receiverReport struct {
	ssrc, mediaSSRC uint32
	fraction        uint8
	lost            uint32
	highest         uint32
	jitter          uint32
	lsr, dlsr       uint32
}

// appendRR appends a compound receiver report with a CNAME.
func appendRR(b []byte, r receiverReport, cname string) []byte {
	b = append(b, 0x81, 201, 0, 7)
	b = binary.BigEndian.AppendUint32(b, r.ssrc)
	b = binary.BigEndian.AppendUint32(b, r.mediaSSRC)
	b = binary.BigEndian.AppendUint32(b, uint32(r.fraction)<<24|min(r.lost, 0x7FFFFF))
	b = binary.BigEndian.AppendUint32(b, r.highest)
	b = binary.BigEndian.AppendUint32(b, r.jitter)
	b = binary.BigEndian.AppendUint32(b, r.lsr)
	b = binary.BigEndian.AppendUint32(b, r.dlsr)
	cname = cname[:min(len(cname), 255)]
	body := 4 + 2 + len(cname) + 1
	pad := (4 - body%4) % 4
	words := (body + pad) / 4
	b = append(b, 0x81, 202)
	b = binary.BigEndian.AppendUint16(b, uint16(words))
	b = binary.BigEndian.AppendUint32(b, r.ssrc)
	b = append(b, 1, byte(len(cname)))
	b = append(b, cname...)
	b = append(b, 0)
	for i := 0; i < pad; i++ {
		b = append(b, 0)
	}
	return b
}
