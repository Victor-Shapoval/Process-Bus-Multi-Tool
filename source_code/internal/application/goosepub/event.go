package goosepub

// FrameSink receives encoded GOOSE frames for transmission over the network.
type FrameSink interface {
	WriteFrame(frame []byte) error
}
