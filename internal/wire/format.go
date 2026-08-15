// Package wire owns this app's own length-prefixed access-unit format and
// everything that operates purely on it: the framing itself, the fan-out that
// delivers framed units to concurrently-connected HTTP viewers, and the demux
// pipeline that turns a camera's raw frames into framed units.
//
// Nothing in this package imports the Hikvision SDK, and that is the point.
// Package main cannot be built or tested without the vendored proprietary
// headers and a Windows toolchain, which meant this logic - a mutex, a map of
// channels, and some byte framing, none of it SDK-dependent - was untestable on
// any other machine. The SDK-touching work (login, RealPlay, codec sniffing)
// stays in package main and feeds frames in through Pipeline's type parameter.
package wire

import "encoding/binary"

// HeaderSize is the fixed header this format prepends to every access unit:
// 1 flags byte (bit 0 = keyframe) + 4-byte big-endian payload length.
// Deliberately minimal - no container format, no ffmpeg - the frontend (see
// frontend/src/hooks/useCameraStream.ts) parses this directly and hands the
// payload straight to WebCodecs' EncodedVideoChunk.
//
// Note that this format is implemented twice, in two languages: Encode below
// writes it and extractMessages in useCameraStream.ts reads it. Neither side
// can validate the other, so a framing change must land in both at once - a
// mismatch surfaces as an opaque decoder failure, not a parse error.
const HeaderSize = 5

// FlagKeyframe is bit 0 of the header's flags byte.
const FlagKeyframe = 0x01

// Encode frames one access unit into a newly allocated wire message. A fresh
// slice per call is load-bearing, not incidental: Broadcaster hands the same
// slice to every subscriber and caches keyframes indefinitely for reseeding, so
// a reused or later-mutated buffer would corrupt already-delivered units.
func Encode(data []byte, keyframe bool) []byte {
	msg := make([]byte, HeaderSize+len(data))
	if keyframe {
		msg[0] = FlagKeyframe
	}
	binary.BigEndian.PutUint32(msg[1:5], uint32(len(data)))
	copy(msg[HeaderSize:], data)
	return msg
}

// MessageIsKeyframe reports whether an already-encoded wire message carries the
// keyframe flag. Safe on a short or empty slice, which it reports as false.
func MessageIsKeyframe(msg []byte) bool {
	return len(msg) > 0 && msg[0]&FlagKeyframe != 0
}
