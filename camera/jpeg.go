package camera

import "bytes"

// jpegWhole reports whether a JPEG image runs to its EOI marker. A frame cut
// short (lost USB packets) still parses as a header. Zero bytes after the
// marker, which some cameras pad frames with, are allowed.
func jpegWhole(b []byte) bool {
	b = bytes.TrimRight(b, "\x00")
	return len(b) >= 4 && b[0] == 0xFF && b[1] == 0xD8 && b[len(b)-2] == 0xFF && b[len(b)-1] == 0xD9
}
