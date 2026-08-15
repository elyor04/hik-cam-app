package wire

import (
	"bytes"
	"testing"
)

func TestEncodeRoundTrip(t *testing.T) {
	payload := []byte{0x00, 0x00, 0x01, 0x65, 0xDE, 0xAD}

	for _, keyframe := range []bool{true, false} {
		msg := Encode(payload, keyframe)

		if got, want := len(msg), HeaderSize+len(payload); got != want {
			t.Fatalf("keyframe=%v: encoded length = %d, want %d", keyframe, got, want)
		}
		if got := MessageIsKeyframe(msg); got != keyframe {
			t.Errorf("keyframe=%v: MessageIsKeyframe = %v", keyframe, got)
		}
		// Big-endian length in bytes 1..4.
		gotLen := int(msg[1])<<24 | int(msg[2])<<16 | int(msg[3])<<8 | int(msg[4])
		if gotLen != len(payload) {
			t.Errorf("keyframe=%v: header length = %d, want %d", keyframe, gotLen, len(payload))
		}
		if !bytes.Equal(msg[HeaderSize:], payload) {
			t.Errorf("keyframe=%v: payload not preserved", keyframe)
		}
	}
}

// TestEncodeAllocatesFreshSlice guards the property Broadcaster's keyframe cache
// and its hand-the-same-slice-to-everyone fan-out both depend on: Encode must
// never hand back a buffer it will reuse or that aliases the caller's input.
func TestEncodeAllocatesFreshSlice(t *testing.T) {
	payload := []byte{1, 2, 3, 4}
	first := Encode(payload, true)
	second := Encode(payload, true)

	if &first[0] == &second[0] {
		t.Fatal("two Encode calls returned the same backing array")
	}

	// Mutating the caller's input must not disturb an already-encoded message.
	payload[0] = 0xFF
	if first[HeaderSize] != 1 {
		t.Fatal("encoded message aliases the caller's payload slice")
	}
}

func TestMessageIsKeyframeShortInput(t *testing.T) {
	if MessageIsKeyframe(nil) {
		t.Error("nil message reported as a keyframe")
	}
	if MessageIsKeyframe([]byte{}) {
		t.Error("empty message reported as a keyframe")
	}
}
