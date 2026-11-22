package gaal

import (
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// This file holds the only network access in the package.

// ohPin identifies one exact file published by Cisco. bzSHA256 covers the
// download and is checked before decompression. rawSHA256 covers the library,
// is the value in Cisco's .signed.sha256.txt file, and is checked before every
// load.
type ohPin struct {
	file      string // library name, the download adds ".bz2"
	bzSize    int64
	bzSHA256  string
	rawSize   int64
	rawSHA256 string
}

// ohVersion is the release in ohPins. It must have the ABI of openh264_layout.go.
const ohVersion = "2.7.0"

// ohBaseURL is Cisco's distribution server. Plain HTTP is safe here only because
// every byte is checked against ohPins.
var ohBaseURL = "http://ciscobinary.openh264.org"

var ohPins = map[string]ohPin{
	"linux/amd64": {"libopenh264-2.7.0-linux64.8.so",
		628667, "44ad0dd2c5af2bac84d26f89f2eb4e268708b8739323d15d33837f7221ce4980",
		1701040, "1522f873a482923c5ec018a4415e1866898197528828c50f90fd300c94620df3"},
	"linux/arm64": {"libopenh264-2.7.0-linux-arm64.8.so",
		629040, "74c7a1d02003d80daf2b4bc7bbb02f73e5548debd4c158b495e1f83d80ebd136",
		1505912, "9ac664b0154fd0868491c2a904397b8a05779874c0603b8b1f16ad15de1ffd82"},
	"darwin/arm64": {"libopenh264-2.7.0-mac-arm64.dylib",
		467536, "c9f2338359093a3e0628e01717be373c6948994a6070a706d303f052013e2b5f",
		1208304, "2e1ae3cd10c08328ca03a1f304fd2a1a2a7e5658c6d5a8582101a963f7995ef0"},
	"windows/amd64": {"openh264-2.7.0-win64.dll",
		456237, "7073fda55e904ac515f787d9728c4bcf9965ec7c91c9f84eae3f260ef9b9b37c",
		975352, "0ef5e7f3b38753b15c13afa0418d4d7805fa228e6adfd3e425bbf758cff1c288"},
}

func ohCurrentPin() (ohPin, bool) {
	p, ok := ohPins[runtime.GOOS+"/"+runtime.GOARCH]
	return p, ok
}

func ohDownloadable() bool {
	_, ok := ohCurrentPin()
	return ok
}

func ohCacheDir(l *Library) (string, error) {
	if l.opts.CacheDir != "" {
		return l.opts.CacheDir, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gaal"), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ohCachedFile returns the path of the verified cached library. The path is
// empty if no copy is cached. A copy that fails verification is an error.
func ohCachedFile(l *Library) (string, error) {
	pin, ok := ohCurrentPin()
	if !ok {
		return "", nil
	}
	dir, err := ohCacheDir(l)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, pin.file)
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.Size() != pin.rawSize {
		return "", fmt.Errorf("openh264: cached %s has the wrong size", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if sha256Hex(b) != pin.rawSHA256 {
		return "", fmt.Errorf("openh264: cached %s fails SHA-256 verification", path)
	}
	return path, nil
}

// Prepare downloads Cisco's OpenH264 binary into the cache so that H.264 can be
// opened on a system without a usable codec. It is the only function in this
// package that uses the network. It returns an error unless
// Options.AllowDownload is set, and it does nothing if a verified copy is
// already cached. The download is checked against SHA-256 values built into this
// package before it is decompressed or stored. Android and iOS are refused.
//
// Applications that use this binary must meet Cisco's licence conditions, see
// http://www.openh264.org/BINARY_LICENSE.txt. Call Prepare on the end user's
// device, because the binary must not be bundled with the application. Let the
// user enable and disable it, which maps to Options.AllowDownload, and show
// "OpenH264 Video Codec provided by Cisco Systems, Inc." at that control.
// Reproduce the licence text with the application's licensing information.
func (l *Library) Prepare(ctx context.Context, c Codec) error {
	if c != H264 {
		return fmt.Errorf("%w: no downloadable codec for %s", ErrInvalid, c)
	}
	if !l.opts.AllowDownload {
		return fmt.Errorf("%w: Options.AllowDownload is false, so OpenH264 will not be downloaded", ErrInvalid)
	}
	if runtime.GOOS == "android" || runtime.GOOS == "ios" {
		return fmt.Errorf("gaal: OpenH264 cannot be downloaded on %s", runtime.GOOS)
	}
	pin, ok := ohCurrentPin()
	if !ok {
		return fmt.Errorf("gaal: Cisco publishes no pinned OpenH264 binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if path, err := ohCachedFile(l); err == nil && path != "" {
		return nil
	}
	dir, err := ohCacheDir(l)
	if err != nil {
		return fmt.Errorf("gaal: no cache directory for OpenH264: %w", err)
	}
	raw, err := ohFetch(ctx, pin)
	if err != nil {
		return err
	}
	return ohStore(dir, pin.file, raw)
}

// ohFetch downloads and verifies a pinned file and returns the library.
func ohFetch(ctx context.Context, pin ohPin) ([]byte, error) {
	url := ohBaseURL + "/" + pin.file + ".bz2"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// A redirect would leave the pinned URL, so it is treated as a failure.
	client := &http.Client{
		Timeout:       5 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openh264: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openh264: download %s: %s", url, resp.Status)
	}
	bz, err := io.ReadAll(io.LimitReader(resp.Body, pin.bzSize+1))
	if err != nil {
		return nil, fmt.Errorf("openh264: download: %w", err)
	}
	if int64(len(bz)) != pin.bzSize || sha256Hex(bz) != pin.bzSHA256 {
		return nil, fmt.Errorf("openh264: %s fails SHA-256 verification", url)
	}
	raw, err := io.ReadAll(io.LimitReader(bzip2.NewReader(bytes.NewReader(bz)), pin.rawSize+1))
	if err != nil {
		return nil, fmt.Errorf("openh264: decompress: %w", err)
	}
	if int64(len(raw)) != pin.rawSize || sha256Hex(raw) != pin.rawSHA256 {
		return nil, fmt.Errorf("openh264: library in %s fails SHA-256 verification", url)
	}
	return raw, nil
}

// ohStore writes the file atomically. Only verified data reaches this function.
func ohStore(dir, name string, data []byte) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".openh264-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(0o700); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}
