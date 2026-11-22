// Package camera captures video from the platform's native camera API without
// C code: V4L2 on Linux and the NDK Camera2 API on Android.
package camera

import (
	"errors"
	"time"

	"github.com/aep/gaal"
)

// ErrUnsupported is returned on platforms without a camera backend.
var ErrUnsupported = errors.New("camera: not supported on this platform")

// ErrClosed is returned by a capture call that the camera's Close ended.
var ErrClosed = errors.New("camera: closed")

// Config selects a capture mode. The camera may pick the nearest size.
type Config struct {
	// Device is the camera to open: a /dev/videoN path on Linux. Empty opens
	// the first one that works.
	Device             string
	Width, Height, FPS int
	Library            *gaal.Library // decodes compressed camera formats
	// KeepJPEG makes Capture hand out the captures of an MJPEG or JPEG device
	// undecoded. Next still decodes them.
	KeepJPEG bool
}

// Device is a capture device found by Devices.
type Device struct {
	Path   string // node to pass as Config.Device, such as /dev/video0
	Name   string // card name the driver reports
	Driver string
	Bus    string // bus_info, such as usb-0000:00:14.0-1
}

// Devices lists the streaming capture devices with a pixel format this
// package reads, by node number.
func Devices() ([]Device, error) { return devices() }

// Capture is one captured picture. Exactly one of Frame and JPEG is set.
type Capture struct {
	// Frame is a raw or decoded picture, which the caller must Release.
	Frame *gaal.VideoFrame
	// JPEG is a complete JFIF image, Huffman tables included, from a device
	// capturing MJPEG with Config.KeepJPEG. It is valid until the next
	// capture call or Close.
	JPEG          []byte
	Width, Height int
	// Captured is the wall-clock time the driver captured the picture, or the
	// time it was dequeued when the driver does not stamp buffers.
	Captured time.Time
}

// Capturer is implemented by cameras that report capture times and can hand
// out JPEG as captured.
type Capturer interface {
	Capture(timeout time.Duration) (Capture, error)
}

func (c Config) withDefaults() Config {
	if c.Width == 0 {
		c.Width, c.Height = 1280, 720
	}
	if c.FPS == 0 {
		c.FPS = 30
	}
	if c.Library == nil {
		c.Library = gaal.New(gaal.Options{})
	}
	return c
}

// Camera is an open capture device. It is not safe for concurrent use.
type Camera interface {
	// Next returns the next frame, which the caller must Release.
	Next(timeout time.Duration) (*gaal.VideoFrame, error)
	// Name describes the device and capture format.
	Name() string
	Close() error
}

// Open starts the default camera.
func Open(cfg Config) (Camera, error) { return open(cfg.withDefaults()) }
