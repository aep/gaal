//go:build !linux && !android && !darwin && !windows

package display

func platformBackends() []opener { return nil }
