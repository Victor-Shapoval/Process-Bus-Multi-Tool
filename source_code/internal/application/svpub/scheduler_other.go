//go:build !linux

package svpub

// Preserve the existing timer implementation on macOS and other platforms.
func newPublisherScheduler() *monotonicScheduler { return newMonotonicScheduler() }

func (*monotonicScheduler) Err() error { return nil }
