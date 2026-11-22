//go:build !((linux || android) && (amd64 || arm64))

package camera

func open(Config) (Camera, error) { return nil, ErrUnsupported }

func devices() ([]Device, error) { return nil, ErrUnsupported }
