//go:build linux && !android && (amd64 || arm64)

package gaal

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/aep/gaal/internal/dl"
)

// Direct VA-API through libva and libva-drm. All layouts come from
// vaapi_layout_linux.go. A display is opened once per render node and shared by
// every stream, because initialising a driver is slow and libva serialises
// calls per display. Streams only own their config, context, surfaces and
// buffers.

type vaAPI struct {
	getDisplayDRM, setErrorCB, setInfoCB, initialize, terminate, errorStr,
	maxProfiles, maxEntrypoints, queryProfiles, queryEntrypoints, getAttribs,
	createConfig, destroyConfig, createSurfaces, destroySurfaces,
	createContext, destroyContext, createBuffer, destroyBuffer,
	mapBuffer, mapBuffer2, unmapBuffer, begin, render, end, syncSurface,
	deriveImage, createImage, destroyImage, getImage, putImage uintptr
}

var loadVA = sync.OnceValues(func() (*vaAPI, error) {
	h, err := dl.Open(libNames("va", "2")...)
	if err != nil {
		return nil, err
	}
	drm, err := dl.Open(libNames("va-drm", "2")...)
	if err != nil {
		return nil, err
	}
	a := &vaAPI{}
	if err := dl.BindAll(drm, dl.Table{"vaGetDisplayDRM": &a.getDisplayDRM}); err != nil {
		return nil, err
	}
	if err := dl.BindAll(h, dl.Table{
		"vaSetErrorCallback": &a.setErrorCB, "vaSetInfoCallback": &a.setInfoCB,
		"vaInitialize": &a.initialize, "vaTerminate": &a.terminate, "vaErrorStr": &a.errorStr,
		"vaMaxNumProfiles": &a.maxProfiles, "vaMaxNumEntrypoints": &a.maxEntrypoints,
		"vaQueryConfigProfiles": &a.queryProfiles, "vaQueryConfigEntrypoints": &a.queryEntrypoints,
		"vaGetConfigAttributes": &a.getAttribs, "vaCreateConfig": &a.createConfig,
		"vaDestroyConfig": &a.destroyConfig, "vaCreateSurfaces": &a.createSurfaces,
		"vaDestroySurfaces": &a.destroySurfaces, "vaCreateContext": &a.createContext,
		"vaDestroyContext": &a.destroyContext, "vaCreateBuffer": &a.createBuffer,
		"vaDestroyBuffer": &a.destroyBuffer, "vaMapBuffer": &a.mapBuffer, "vaUnmapBuffer": &a.unmapBuffer,
		"vaBeginPicture": &a.begin, "vaRenderPicture": &a.render, "vaEndPicture": &a.end,
		"vaSyncSurface": &a.syncSurface, "vaDeriveImage": &a.deriveImage,
		"vaCreateImage": &a.createImage, "vaDestroyImage": &a.destroyImage,
		"vaGetImage": &a.getImage, "vaPutImage": &a.putImage,
	}); err != nil {
		return nil, err
	}
	// vaMapBuffer2 is newer (libva 2.21). Without it mapping is read-write.
	a.mapBuffer2, _ = dl.Sym(h, "vaMapBuffer2")
	return a, nil
})

// vaHeap allocates memory for C to fill. A value whose address only reaches C as
// a uintptr does not escape, so it could live on the goroutine stack and move
// when the stack grows. Not being inlined forces it onto the heap.
//
//go:noinline
func vaHeap[T any](n int) []T { return make([]T, n) }

type vaEndpoint struct{ profile, entry int32 }

// vaDisplay is an initialised render node.
type vaDisplay struct {
	path string
	fd   int
	dpy  uintptr
	caps map[vaEndpoint]bool
}

func (d *vaDisplay) has(profile, entry int32) bool { return d.caps[vaEndpoint{profile, entry}] }

// vaProfilesOfInterest are the profiles whose entrypoints are recorded.
var vaProfilesOfInterest = []int32{vaProfileH264CB, vaProfileH264Main, vaProfileH264High, vaProfileJPEG,
	vaProfileHEVCMain, vaProfileVP9P0}

func (a *vaAPI) statusText(st int32) string {
	r, _, _ := purego.SyscallN(a.errorStr, uintptr(uint32(st)))
	return dl.GoString(r)
}

func (a *vaAPI) openNode(path string) (*vaDisplay, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var c dl.Caller
	dpy := c.C1(a.getDisplayDRM, uintptr(fd))
	if dpy == 0 {
		syscall.Close(fd)
		return nil, errors.New("vaGetDisplayDRM failed")
	}
	// libva logs to stdout and stderr by default. Failures are returned instead.
	c.C3(a.setErrorCB, dpy, 0, 0)
	c.C3(a.setInfoCB, dpy, 0, 0)
	d := &vaDisplay{path: path, fd: fd, dpy: dpy, caps: map[vaEndpoint]bool{}}
	fail := func(err error) (*vaDisplay, error) {
		c.C1(a.terminate, dpy)
		syscall.Close(fd)
		return nil, err
	}
	out := vaHeap[int32](2) // major and minor version, then list lengths
	if st := int32(c.C3(a.initialize, dpy, uintptr(unsafe.Pointer(&out[0])), uintptr(unsafe.Pointer(&out[1])))); st != 0 {
		return fail(fmt.Errorf("vaInitialize: %s", a.statusText(st)))
	}
	profiles := vaHeap[int32](max(int(int32(c.C1(a.maxProfiles, dpy))), 1))
	if st := int32(c.C3(a.queryProfiles, dpy, uintptr(unsafe.Pointer(&profiles[0])), uintptr(unsafe.Pointer(&out[0])))); st != 0 {
		return fail(fmt.Errorf("vaQueryConfigProfiles: %s", a.statusText(st)))
	}
	n := out[0]
	entries := vaHeap[int32](max(int(int32(c.C1(a.maxEntrypoints, dpy))), 1))
	for _, p := range profiles[:n] {
		interesting := false
		for _, q := range vaProfilesOfInterest {
			interesting = interesting || p == q
		}
		if !interesting {
			continue
		}
		if st := int32(c.C5(a.queryEntrypoints, dpy, uintptr(uint32(p)), uintptr(unsafe.Pointer(&entries[0])), uintptr(unsafe.Pointer(&out[1])), 0)); st != 0 {
			continue
		}
		for _, e := range entries[:out[1]] {
			d.caps[vaEndpoint{p, e}] = true
		}
	}
	if len(d.caps) == 0 {
		return fail(errors.New("no usable profile"))
	}
	return d, nil
}

// vaProbe opens every render node that has a working driver. Nodes without one,
// such as NVIDIA GPUs, are skipped.
var vaProbe = sync.OnceValues(func() ([]*vaDisplay, error) {
	a, err := loadVA()
	if err != nil {
		return nil, err
	}
	nodes, _ := filepath.Glob("/dev/dri/renderD*")
	sort.Strings(nodes)
	var out []*vaDisplay
	var errs []error
	for _, path := range nodes {
		d, err := a.openNode(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		if len(errs) == 0 {
			return nil, errors.New("vaapi: no render node")
		}
		return nil, fmt.Errorf("vaapi: %w", errors.Join(errs...))
	}
	return out, nil
})

// vaFind returns the displays offering any of the profiles at any entrypoint.
func vaFind(profiles, entries []int32) ([]*vaDisplay, error) {
	all, err := vaProbe()
	if err != nil {
		return nil, err
	}
	var out []*vaDisplay
	for _, d := range all {
		if vaPick(d, profiles, entries) != (vaEndpoint{-1, -1}) {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("vaapi: profile not supported by any render node")
	}
	return out, nil
}

// vaPick returns the first supported combination in preference order.
func vaPick(d *vaDisplay, profiles, entries []int32) vaEndpoint {
	for _, p := range profiles {
		for _, e := range entries {
			if d.has(p, e) {
				return vaEndpoint{p, e}
			}
		}
	}
	return vaEndpoint{-1, -1}
}

var (
	vaH264Profiles = []int32{vaProfileH264High, vaProfileH264Main, vaProfileH264CB}
	vaVLD          = []int32{vaEntryVLD}
	vaEncEntries   = []int32{vaEntryEncSlice, vaEntryEncSliceLP}
	vaJPEGProfiles = []int32{vaProfileJPEG}
	vaH265Profiles = []int32{vaProfileHEVCMain}
	vaVP9Profiles  = []int32{vaProfileVP9P0}
)

// lowDelaySetter is implemented by decoders that reorder pictures themselves.
type lowDelaySetter interface{ setLowDelay() }

func vaapiBackends() []backend {
	avail := func(profiles, entries []int32) func(*Library) error {
		return func(*Library) error {
			_, err := vaFind(profiles, entries)
			return err
		}
	}
	dec := func(c Codec, profiles []int32, open func(BackendInfo) (VideoDecoder, error)) backend {
		info := BackendInfo{"vaapi", c, Decode, true}
		return backend{BackendInfo: info, avail: avail(profiles, vaVLD),
			vdec: func(_ *Library, cfg VideoConfig) (VideoDecoder, error) {
				d, err := open(info)
				if l, ok := d.(lowDelaySetter); ok && err == nil && cfg.LowDelay {
					l.setLowDelay()
				}
				return d, err
			}}
	}
	encInfo := BackendInfo{"vaapi", H264, Encode, true}
	enc265Info := BackendInfo{"vaapi", H265, Encode, true}
	return []backend{
		dec(H264, vaH264Profiles, newVAH264Decoder),
		dec(H265, vaH265Profiles, newVAH265Decoder),
		dec(VP9, vaVP9Profiles, newVAVP9Decoder),
		dec(JPEG, vaJPEGProfiles, newVAJPEGDecoder),
		{BackendInfo: encInfo, avail: avail(vaH264Profiles, vaEncEntries),
			venc: func(_ *Library, cfg VideoConfig) (VideoEncoder, error) { return newVAH264Encoder(cfg, encInfo) }},
		{BackendInfo: enc265Info, avail: avail(vaH265Profiles, vaEncEntries),
			venc: func(_ *Library, cfg VideoConfig) (VideoEncoder, error) { return newVAH265Encoder(cfg, enc265Info) }},
	}
}

// vaSession holds the per-stream VA objects and reusable call arguments. It
// lives on the heap, so pointers to its fields stay valid during C calls.
type vaSession struct {
	c    dl.Caller
	a    *vaAPI
	disp *vaDisplay
	dpy  uintptr

	config, ctx       uint32
	hasConfig, hasCtx bool
	surfaces          []uint32
	bufs              []uint32 // buffers of the picture being built

	tmpID  uint32
	mapped uintptr

	image     []byte // VAImage result of vaDeriveImage or vaCreateImage
	readImage []byte // persistent VAImage for vaGetImage
	imageFmt  []byte
	attr      []byte
	noDerive  bool
}

func newVASession(a *vaAPI, d *vaDisplay) *vaSession {
	s := &vaSession{a: a, disp: d, dpy: d.dpy, image: make([]byte, vaSizeImage),
		imageFmt: make([]byte, vaSizeImageFormat), attr: make([]byte, vaSizeSurfaceAttrib)}
	le32(s.imageFmt, vaoIFFourcc, vaFourccNV12)
	le32(s.imageFmt, vaoIFByteOrder, 1) // VA_LSB_FIRST
	le32(s.imageFmt, vaoIFBpp, 12)
	return s
}

func (s *vaSession) fail(what string, st int32) error {
	return fmt.Errorf("vaapi: %s: %s", what, s.a.statusText(st))
}

func (s *vaSession) check(what string, r uintptr) error {
	if st := int32(r); st != 0 {
		return s.fail(what, st)
	}
	return nil
}

// createConfig makes the stream's config from attribute type and value pairs.
func (s *vaSession) createConfig(ep vaEndpoint, attrs ...uint32) error {
	list := vaHeap[byte](vaSizeConfigAttrib * (len(attrs)/2 + 1))
	for i := 0; i < len(attrs); i += 2 {
		le32(list, i/2*vaSizeConfigAttrib+vaoCAType, attrs[i])
		le32(list, i/2*vaSizeConfigAttrib+vaoCAValue, attrs[i+1])
	}
	err := s.check("create config", s.c.C6(s.a.createConfig, s.dpy, uintptr(uint32(ep.profile)), uintptr(uint32(ep.entry)),
		uintptr(unsafe.Pointer(&list[0])), uintptr(len(attrs)/2), uintptr(unsafe.Pointer(&s.config))))
	runtime.KeepAlive(list)
	if err == nil {
		s.hasConfig = true
	}
	return err
}

// attribute returns a config attribute, or vaAttribNotSupported.
func (s *vaSession) attribute(ep vaEndpoint, typ uint32) (uint32, error) {
	list := vaHeap[byte](vaSizeConfigAttrib)
	le32(list, vaoCAType, typ)
	if err := s.check("get config attributes", s.c.C5(s.a.getAttribs, s.dpy, uintptr(uint32(ep.profile)), uintptr(uint32(ep.entry)),
		uintptr(unsafe.Pointer(&list[0])), 1)); err != nil {
		return 0, err
	}
	return rd32(list, vaoCAValue), nil
}

// createSurfaces appends n surfaces of the given format to s.surfaces.
func (s *vaSession) createSurfaces(rt, fourcc uint32, w, h, n int) error {
	ids := vaHeap[uint32](n)
	a := s.attr
	clear(a)
	le32(a, vaoSAType, vaSurfAttrPixelFormat)
	le32(a, vaoSAFlags, vaSurfAttrSetable)
	le32(a, vaoSAValueType, vaGenericInt)
	le32(a, vaoSAValueInt, fourcc)
	if err := s.check("create surfaces", s.c.C8(s.a.createSurfaces, s.dpy, uintptr(rt), uintptr(w), uintptr(h),
		uintptr(unsafe.Pointer(&ids[0])), uintptr(n), uintptr(unsafe.Pointer(&a[0])), 1)); err != nil {
		return err
	}
	s.surfaces = append(s.surfaces, ids...)
	return nil
}

func (s *vaSession) createContext(w, h int, flags uint32) error {
	err := s.check("create context", s.c.C8(s.a.createContext, s.dpy, uintptr(s.config), uintptr(w), uintptr(h), uintptr(flags),
		uintptr(unsafe.Pointer(&s.surfaces[0])), uintptr(len(s.surfaces)), uintptr(unsafe.Pointer(&s.ctx))))
	if err == nil {
		s.hasCtx = true
	}
	return err
}

// destroyVideo frees the context, surfaces and read-back image but keeps the
// config, ready for a format change.
func (s *vaSession) destroyVideo() {
	if s.readImage != nil {
		s.c.C2(s.a.destroyImage, s.dpy, uintptr(rd32(s.readImage, vaoImgID)))
		s.readImage = nil
	}
	if s.hasCtx {
		s.c.C2(s.a.destroyContext, s.dpy, uintptr(s.ctx))
		s.hasCtx = false
	}
	if len(s.surfaces) > 0 {
		s.c.C3(s.a.destroySurfaces, s.dpy, uintptr(unsafe.Pointer(&s.surfaces[0])), uintptr(len(s.surfaces)))
		s.surfaces = s.surfaces[:0]
	}
}

func (s *vaSession) destroyConfig() {
	if s.hasConfig {
		s.c.C2(s.a.destroyConfig, s.dpy, uintptr(s.config))
		s.hasConfig = false
	}
}

func (s *vaSession) close() {
	s.dropBuffers()
	s.destroyVideo()
	s.destroyConfig()
}

// addBuffer creates a buffer holding a copy of size bytes at data, and queues
// it for the next render.
func (s *vaSession) addBuffer(typ uint32, data unsafe.Pointer, size int) error {
	id, err := s.newBuffer(typ, data, size)
	if err != nil {
		return err
	}
	s.bufs = append(s.bufs, id)
	return nil
}

func (s *vaSession) newBuffer(typ uint32, data unsafe.Pointer, size int) (uint32, error) {
	r := s.c.C7(s.a.createBuffer, s.dpy, uintptr(s.ctx), uintptr(typ), uintptr(size), 1,
		uintptr(data), uintptr(unsafe.Pointer(&s.tmpID)))
	if int32(r) != 0 {
		return 0, s.fail("create buffer", int32(r))
	}
	return s.tmpID, nil
}

func (s *vaSession) dropBuffers() {
	for _, id := range s.bufs {
		s.c.C2(s.a.destroyBuffer, s.dpy, uintptr(id))
	}
	s.bufs = s.bufs[:0]
}

// submit sends the queued buffers as one picture targeting surface. Buffers are
// destroyed whatever the outcome.
func (s *vaSession) submit(surface uint32) error {
	defer s.dropBuffers()
	if r := s.c.C3(s.a.begin, s.dpy, uintptr(s.ctx), uintptr(surface)); int32(r) != 0 {
		return s.fail("begin picture", int32(r))
	}
	// There is no four-argument Caller method. The extra register is ignored.
	r := s.c.C5(s.a.render, s.dpy, uintptr(s.ctx), uintptr(unsafe.Pointer(&s.bufs[0])), uintptr(len(s.bufs)), 0)
	// A begun picture must be ended even if rendering failed.
	e := s.c.C2(s.a.end, s.dpy, uintptr(s.ctx))
	if int32(r) != 0 {
		return s.decodeStatus("render picture", int32(r))
	}
	if int32(e) != 0 {
		return s.decodeStatus("end picture", int32(e))
	}
	return nil
}

var errVABitstream = fmt.Errorf("%w: hardware rejected the bitstream", ErrCorrupt)

// decodeStatus separates damaged input from driver failure.
func (s *vaSession) decodeStatus(what string, st int32) error {
	if st == vaStatusDecodingError {
		return errVABitstream
	}
	return s.fail(what, st)
}

func (s *vaSession) sync(surface uint32) error {
	if r := s.c.C2(s.a.syncSurface, s.dpy, uintptr(surface)); int32(r) != 0 {
		return s.decodeStatus("sync surface", int32(r))
	}
	return nil
}

// mapBuf maps a buffer. flags is a VA_MAPBUFFER_FLAG value, used when possible:
// a write-only map avoids reading the surface back first.
func (s *vaSession) mapBuf(id uint32, flags uintptr) (uintptr, error) {
	var r uintptr
	if s.a.mapBuffer2 != 0 {
		r = s.c.C5(s.a.mapBuffer2, s.dpy, uintptr(id), uintptr(unsafe.Pointer(&s.mapped)), flags, 0)
	} else {
		r = s.c.C3(s.a.mapBuffer, s.dpy, uintptr(id), uintptr(unsafe.Pointer(&s.mapped)))
	}
	if int32(r) != 0 {
		return 0, s.fail("map buffer", int32(r))
	}
	return s.mapped, nil
}

func (s *vaSession) unmapBuf(id uint32) { s.c.C2(s.a.unmapBuffer, s.dpy, uintptr(id)) }

// derive maps a surface directly. It reports false when the driver cannot
// expose this surface as NV12, and then remembers that for the stream.
func (s *vaSession) derive(surface uint32) bool {
	if s.noDerive {
		return false
	}
	if int32(s.c.C3(s.a.deriveImage, s.dpy, uintptr(surface), uintptr(unsafe.Pointer(&s.image[0])))) != 0 {
		s.noDerive = true
		return false
	}
	if rd32(s.image, vaoImgFourcc) != vaFourccNV12 {
		s.c.C2(s.a.destroyImage, s.dpy, uintptr(rd32(s.image, vaoImgID)))
		s.noDerive = true
		return false
	}
	return true
}

// vaReadDerive selects mapping decoded surfaces directly, which falls back to
// vaGetImage when the driver cannot. On radeonsi a 1080p read takes 0.64 ms this
// way and 0.79 ms through vaGetImage. The mapping is a cached staging copy, read
// at about 9 GB/s, so a streaming (MOVNTDQA) copy would gain nothing.
var vaReadDerive = true

// readSurface copies a decoded NV12 surface into f, whose size selects the
// visible area starting at (cropX, cropY). The caller has already synced it.
func (s *vaSession) readSurface(surface uint32, f *VideoFrame, cropX, cropY, codedW, codedH int) error {
	if vaReadDerive && s.derive(surface) {
		img := s.image
		id, buf := rd32(img, vaoImgID), rd32(img, vaoImgBuf)
		addr, err := s.mapBuf(buf, vaMapRead)
		if err == nil {
			err = vaCopyOut(addr, img, f, cropX, cropY)
			s.unmapBuf(buf)
		}
		s.c.C2(s.a.destroyImage, s.dpy, uintptr(id))
		return err
	}
	if s.readImage == nil {
		img := make([]byte, vaSizeImage)
		if err := s.check("create image", s.c.C5(s.a.createImage, s.dpy, uintptr(unsafe.Pointer(&s.imageFmt[0])),
			uintptr(codedW), uintptr(codedH), uintptr(unsafe.Pointer(&img[0])))); err != nil {
			return err
		}
		s.readImage = img
	}
	img := s.readImage
	if err := s.check("get image", s.c.C7(s.a.getImage, s.dpy, uintptr(surface), 0, 0, uintptr(codedW), uintptr(codedH),
		uintptr(rd32(img, vaoImgID)))); err != nil {
		return err
	}
	buf := rd32(img, vaoImgBuf)
	addr, err := s.mapBuf(buf, vaMapRead)
	if err != nil {
		return err
	}
	err = vaCopyOut(addr, img, f, cropX, cropY)
	s.unmapBuf(buf)
	return err
}

var errVAImageLayout = errors.New("vaapi: image smaller than the picture")

// vaCopyOut copies the visible NV12 area of a mapped VAImage into f.
func vaCopyOut(addr uintptr, img []byte, f *VideoFrame, cropX, cropY int) error {
	size := int(rd32(img, vaoImgSize))
	yPitch, uvPitch := int(rd32(img, vaoImgPitches)), int(rd32(img, vaoImgPitches+4))
	yOff, uvOff := int(rd32(img, vaoImgOffsets)), int(rd32(img, vaoImgOffsets+4))
	w, h := f.Width, f.Height
	cw, ch := (w+1)/2, (h+1)/2
	yStart := yOff + cropY*yPitch + cropX
	uvStart := uvOff + cropY/2*uvPitch + cropX&^1
	if yStart+(h-1)*yPitch+w > size || uvStart+(ch-1)*uvPitch+2*cw > size {
		return errVAImageLayout
	}
	mem := cMem(addr, size)
	copyPlane(f.Y, f.YStride, mem[yStart:], yPitch, w, h)
	copyPlane(f.U, f.UVStride, mem[uvStart:], uvPitch, 2*cw, ch)
	return nil
}

func vaPut16(b []byte, off int, v uint16) { *(*uint16)(unsafe.Pointer(&b[off])) = v }
