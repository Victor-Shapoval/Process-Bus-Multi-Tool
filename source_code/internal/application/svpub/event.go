package svpub

// FrameSink receives encoded SV frames for transmission over the network.
type FrameSink interface {
	WriteFrame(frame []byte) error
}
