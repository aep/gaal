//go:build android && (amd64 || arm64)

package camera

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/aep/gaal"
	"github.com/aep/gaal/internal/dl"
)

// The NDK Camera2 API, delivering YUV images through an AImageReader. The
// process needs the CAMERA permission and must be a foreground app.

const (
	formatYUV420  = 0x23
	templateVideo = 3 // TEMPLATE_RECORD
	readerImages  = 4
	pollStep      = time.Millisecond
)

type ndkAPI struct {
	managerCreate   func() uintptr
	managerDelete   func(uintptr)
	idList          func(m uintptr, out *uintptr) int32
	deleteIDList    func(uintptr)
	openCamera      func(m uintptr, id *byte, cb unsafe.Pointer, out *uintptr) int32
	closeDevice     func(uintptr) int32
	readerNew       func(w, h, format, images int32, out *uintptr) int32
	readerDelete    func(uintptr)
	readerWindow    func(r uintptr, out *uintptr) int32
	containerCreate func(out *uintptr) int32
	containerFree   func(uintptr)
	containerAdd    func(c, o uintptr) int32
	outputCreate    func(win uintptr, out *uintptr) int32
	outputFree      func(uintptr)
	targetCreate    func(win uintptr, out *uintptr) int32
	targetFree      func(uintptr)
	requestCreate   func(dev uintptr, template int32, out *uintptr) int32
	requestFree     func(uintptr)
	requestAdd      func(req, target uintptr) int32
	sessionCreate   func(dev, container uintptr, cb unsafe.Pointer, out *uintptr) int32
	sessionClose    func(uintptr)
	sessionStop     func(uintptr) int32
	repeating       func(s, cb uintptr, n int32, reqs *uintptr, seq *int32) int32

	acquire, imageDelete, imageW, imageH, planeData, rowStride, pixelStride uintptr

	deviceCB  [3]uintptr
	sessionCB [4]uintptr
}

var loadNDK = sync.OnceValues(func() (*ndkAPI, error) {
	cam, err := dl.Open("libcamera2ndk.so")
	if err != nil {
		return nil, err
	}
	media, err := dl.Open("libmediandk.so")
	if err != nil {
		return nil, err
	}
	a := &ndkAPI{}
	if err := dl.BindAll(cam, dl.Table{
		"ACameraManager_create": &a.managerCreate, "ACameraManager_delete": &a.managerDelete,
		"ACameraManager_getCameraIdList": &a.idList, "ACameraManager_deleteCameraIdList": &a.deleteIDList,
		"ACameraManager_openCamera": &a.openCamera, "ACameraDevice_close": &a.closeDevice,
		"ACaptureSessionOutputContainer_create": &a.containerCreate, "ACaptureSessionOutputContainer_free": &a.containerFree,
		"ACaptureSessionOutputContainer_add": &a.containerAdd, "ACaptureSessionOutput_create": &a.outputCreate,
		"ACaptureSessionOutput_free": &a.outputFree, "ACameraOutputTarget_create": &a.targetCreate,
		"ACameraOutputTarget_free": &a.targetFree, "ACameraDevice_createCaptureRequest": &a.requestCreate,
		"ACaptureRequest_free": &a.requestFree, "ACaptureRequest_addTarget": &a.requestAdd,
		"ACameraDevice_createCaptureSession": &a.sessionCreate, "ACameraCaptureSession_close": &a.sessionClose,
		"ACameraCaptureSession_stopRepeating": &a.sessionStop, "ACameraCaptureSession_setRepeatingRequest": &a.repeating,
	}); err != nil {
		return nil, err
	}
	if err := dl.BindAll(media, dl.Table{
		"AImageReader_new": &a.readerNew, "AImageReader_delete": &a.readerDelete, "AImageReader_getWindow": &a.readerWindow,
		"AImageReader_acquireLatestImage": &a.acquire, "AImage_delete": &a.imageDelete,
		"AImage_getWidth": &a.imageW, "AImage_getHeight": &a.imageH, "AImage_getPlaneData": &a.planeData,
		"AImage_getPlaneRowStride": &a.rowStride, "AImage_getPlanePixelStride": &a.pixelStride,
	}); err != nil {
		return nil, err
	}
	// The NDK requires these callbacks to be present. Failures show up as a
	// camera that stops delivering images, which Next reports as a timeout.
	two := purego.NewCallback(func(ctx, obj uintptr) uintptr { return 0 })
	three := purego.NewCallback(func(ctx, obj, code uintptr) uintptr { return 0 })
	a.deviceCB = [3]uintptr{0, two, three}
	a.sessionCB = [4]uintptr{0, two, two, two}
	return a, nil
})

type ndkCamera struct {
	c   dl.Caller
	api *ndkAPI

	manager, device, reader, window, container, output, target, request, session uintptr

	name   string
	frames gaal.FramePool
	start  time.Time

	image, base, size uintptr
	v32               int32
}

func open(cfg Config) (Camera, error) {
	a, err := loadNDK()
	if err != nil {
		return nil, err
	}
	c := &ndkCamera{api: a, start: time.Now()}
	if err := c.setup(cfg); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func check(what string, status int32) error {
	if status != 0 {
		return fmt.Errorf("camera: %s: status %d", what, status)
	}
	return nil
}

func (c *ndkCamera) setup(cfg Config) error {
	a := c.api
	c.manager = a.managerCreate()
	var list uintptr
	if err := check("list cameras", a.idList(c.manager, &list)); err != nil {
		return err
	}
	defer a.deleteIDList(list)
	// struct ACameraIdList { int numCameras; const char **cameraIds; }
	type idList struct {
		n   int32
		ids **byte
	}
	l := *(**idList)(unsafe.Pointer(&list))
	if l.n == 0 {
		return errors.New("camera: no cameras")
	}
	id := *l.ids
	if err := check("open camera", a.openCamera(c.manager, id, unsafe.Pointer(&a.deviceCB), &c.device)); err != nil {
		return err
	}
	if err := check("create reader", a.readerNew(int32(cfg.Width), int32(cfg.Height), formatYUV420, readerImages, &c.reader)); err != nil {
		return err
	}
	if err := check("reader window", a.readerWindow(c.reader, &c.window)); err != nil {
		return err
	}
	for _, s := range []struct {
		what string
		rc   func() int32
	}{
		{"create container", func() int32 { return a.containerCreate(&c.container) }},
		{"create output", func() int32 { return a.outputCreate(c.window, &c.output) }},
		{"add output", func() int32 { return a.containerAdd(c.container, c.output) }},
		{"create target", func() int32 { return a.targetCreate(c.window, &c.target) }},
		{"create request", func() int32 { return a.requestCreate(c.device, templateVideo, &c.request) }},
		{"add target", func() int32 { return a.requestAdd(c.request, c.target) }},
		{"create session", func() int32 {
			return a.sessionCreate(c.device, c.container, unsafe.Pointer(&a.sessionCB), &c.session)
		}},
		{"start capture", func() int32 { return a.repeating(c.session, 0, 1, &c.request, nil) }},
	} {
		if err := check(s.what, s.rc()); err != nil {
			return err
		}
	}
	c.name = fmt.Sprintf("android camera %s %dx%d", cString(id), cfg.Width, cfg.Height)
	return nil
}

func cString(p *byte) string {
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

func (c *ndkCamera) Name() string { return c.name }

func cBytes(addr uintptr, n int) []byte {
	return unsafe.Slice(*(**byte)(unsafe.Pointer(&addr)), n)
}

func (c *ndkCamera) query(fn, _ uintptr) int {
	c.c.C2(fn, c.image, uintptr(unsafe.Pointer(&c.v32)))
	return int(c.v32)
}

func (c *ndkCamera) planeInt(fn uintptr, plane int) int {
	c.c.C3(fn, c.image, uintptr(plane), uintptr(unsafe.Pointer(&c.v32)))
	return int(c.v32)
}

func (c *ndkCamera) plane(i int) []byte {
	c.v32 = 0
	c.c.C5(c.api.planeData, c.image, uintptr(i), uintptr(unsafe.Pointer(&c.base)), uintptr(unsafe.Pointer(&c.v32)), 0)
	return cBytes(c.base, int(c.v32))
}

func (c *ndkCamera) Next(timeout time.Duration) (*gaal.VideoFrame, error) {
	a := c.api
	deadline := time.Now().Add(timeout)
	for {
		if int32(c.c.C2(a.acquire, c.reader, uintptr(unsafe.Pointer(&c.image)))) == 0 && c.image != 0 {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("camera: timed out (is the app in the foreground?)")
		}
		time.Sleep(pollStep)
	}
	defer func() {
		c.c.C1(a.imageDelete, c.image)
		c.image = 0
	}()
	w, h := c.query(a.imageW, 0), c.query(a.imageH, 0)
	f := c.frames.Get(w, h, gaal.NV12)
	f.PTS = time.Since(c.start)
	ys := c.planeInt(a.rowStride, 0)
	y := c.plane(0)
	for r := 0; r < h; r++ {
		copy(f.Y[r*f.YStride:r*f.YStride+w], y[r*ys:])
	}
	us, up := c.planeInt(a.rowStride, 1), c.planeInt(a.pixelStride, 1)
	vs, vp := c.planeInt(a.rowStride, 2), c.planeInt(a.pixelStride, 2)
	u, v := c.plane(1), c.plane(2)
	cw, ch := (w+1)/2, (h+1)/2
	semi := up == 2 && vp == 2 && us == vs && len(v) > 0 && uintptr(unsafe.Pointer(&v[0])) == uintptr(unsafe.Pointer(&u[0]))+1
	for r := 0; r < ch; r++ {
		dst := f.U[r*f.UVStride : r*f.UVStride+2*cw]
		if semi {
			copy(dst, u[r*us:])
			continue
		}
		for x := 0; x < cw; x++ {
			dst[2*x], dst[2*x+1] = u[r*us+x*up], v[r*vs+x*vp]
		}
	}
	runtime.KeepAlive(c)
	return f, nil
}

func (c *ndkCamera) Close() error {
	a := c.api
	if c.session != 0 {
		a.sessionStop(c.session)
		a.sessionClose(c.session)
		c.session = 0
	}
	if c.request != 0 {
		a.requestFree(c.request)
		c.request = 0
	}
	if c.target != 0 {
		a.targetFree(c.target)
		c.target = 0
	}
	if c.output != 0 {
		a.outputFree(c.output)
		c.output = 0
	}
	if c.container != 0 {
		a.containerFree(c.container)
		c.container = 0
	}
	if c.device != 0 {
		a.closeDevice(c.device)
		c.device = 0
	}
	if c.reader != 0 {
		a.readerDelete(c.reader)
		c.reader = 0
	}
	if c.manager != 0 {
		a.managerDelete(c.manager)
		c.manager = 0
	}
	return nil
}

// devices is not implemented for Camera2 yet: Open picks the back camera.
func devices() ([]Device, error) { return nil, ErrUnsupported }
