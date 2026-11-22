//go:build darwin && amd64

package gaal

// gaal supports Apple silicon only. Intel Macs pass CMTime differently, so the
// native backend never ran there, and they are no longer built or tested.
var _ = gaal_requires_apple_silicon
