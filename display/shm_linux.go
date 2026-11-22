//go:build linux && !android

package display

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	mfdCloexec      = 1
	mfdAllowSealing = 2
	fAddSeals       = 1033
	fSealShrink     = 2
)

func memfdSyscall() uintptr {
	switch runtime.GOARCH {
	case "amd64":
		return 319
	case "386":
		return 356
	case "arm":
		return 385
	case "ppc64", "ppc64le":
		return 360
	case "s390x":
		return 350
	case "arm64", "riscv64", "loong64":
		return 279
	}
	return 0
}

// sharedFile returns an unnamed file that can be mapped and passed to another
// process. sealable asks for a memfd, which udmabuf needs.
func sharedFile(size int, sealable bool) (int, error) {
	fd := -1
	if nr := memfdSyscall(); nr != 0 {
		name := [...]byte{'g', 'a', 'a', 'l', 0}
		flags := uintptr(mfdCloexec)
		if sealable {
			flags |= mfdAllowSealing
		}
		r, _, e := syscall.Syscall(nr, uintptr(unsafe.Pointer(&name[0])), flags, 0)
		if e == 0 {
			fd = int(r)
		}
	}
	if fd < 0 {
		if sealable {
			return -1, fmt.Errorf("memfd is not available")
		}
		for _, dir := range []string{os.Getenv("XDG_RUNTIME_DIR"), "/dev/shm", os.TempDir()} {
			if dir == "" {
				continue
			}
			f, err := os.CreateTemp(dir, "gaal-display-*")
			if err != nil {
				continue
			}
			os.Remove(f.Name())
			var e error
			fd, e = syscall.Dup(int(f.Fd()))
			f.Close()
			if e == nil {
				syscall.CloseOnExec(fd)
				break
			}
			fd = -1
		}
		if fd < 0 {
			return -1, fmt.Errorf("cannot create a shared file")
		}
	}
	if err := syscall.Ftruncate(fd, int64(size)); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

// shmem is a mapped shared file.
type shmem struct {
	fd   int
	data []byte
}

func newShmem(size int, sealable bool) (*shmem, error) {
	fd, err := sharedFile(size, sealable)
	if err != nil {
		return nil, err
	}
	data, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return &shmem{fd, data}, nil
}

func (m *shmem) free() {
	if m == nil {
		return
	}
	syscall.Munmap(m.data)
	syscall.Close(m.fd)
	m.data = nil
}

// wbuf builds wire messages. It is shared by the X11 and Wayland code.
type wbuf struct{ b []byte }
