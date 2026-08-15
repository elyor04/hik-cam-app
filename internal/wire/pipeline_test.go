package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// testFrame stands in for hikvision.Frame - the whole reason Pipeline is generic
// over its frame type is so this file can exist without the SDK.
type testFrame struct {
	data    []byte
	sysHead bool
}

func splitTestFrame(f testFrame) ([]byte, bool) { return f.data, f.sysHead }

// annexB builds a single-NAL Annex-B access unit of the given nal_unit_type.
// Type 5 is an IDR slice (keyframe), type 1 a non-IDR slice.
func annexB(nalType byte, payload ...byte) []byte {
	return append([]byte{0x00, 0x00, 0x01, nalType}, payload...)
}

// decodeMessages parses a stream of wire messages back into payloads, which is
// also an end-to-end check that Encode's framing is self-consistent.
func decodeMessages(t *testing.T, buf []byte) (payloads [][]byte, keyframes []bool) {
	t.Helper()
	for len(buf) > 0 {
		if len(buf) < HeaderSize {
			t.Fatalf("trailing %d bytes are too short for a wire header", len(buf))
		}
		n := int(binary.BigEndian.Uint32(buf[1:5]))
		if len(buf) < HeaderSize+n {
			t.Fatalf("message claims %d payload bytes, only %d remain", n, len(buf)-HeaderSize)
		}
		keyframes = append(keyframes, MessageIsKeyframe(buf))
		payloads = append(payloads, buf[HeaderSize:HeaderSize+n])
		buf = buf[HeaderSize+n:]
	}
	return payloads, keyframes
}

func TestPipelineESWritesBufferedThenStreamedFrames(t *testing.T) {
	key := annexB(5, 'k')
	delta := annexB(1, 'd')

	frames := make(chan testFrame, 2)
	frames <- testFrame{data: delta}
	frames <- testFrame{data: key}
	close(frames)

	var out bytes.Buffer
	p := Pipeline[testFrame]{Mode: ModeES, Split: splitTestFrame}
	if err := p.Run([]testFrame{{data: key}}, frames, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	payloads, keyframes := decodeMessages(t, out.Bytes())
	if len(payloads) != 3 {
		t.Fatalf("expected 3 units (1 buffered + 2 streamed), got %d", len(payloads))
	}
	if !bytes.Equal(payloads[0], key) || !bytes.Equal(payloads[1], delta) || !bytes.Equal(payloads[2], key) {
		t.Error("units were not written in buffered-then-streamed order")
	}
	if want := []bool{true, false, true}; !equalBools(keyframes, want) {
		t.Errorf("keyframe flags = %v, want %v", keyframes, want)
	}
}

// TestPipelineESSkipsEmptyFrames guards that a zero-length SDK frame is dropped
// rather than emitted as a zero-payload wire message the frontend would have to
// tolerate.
func TestPipelineESSkipsEmptyFrames(t *testing.T) {
	frames := make(chan testFrame, 2)
	frames <- testFrame{data: nil}
	frames <- testFrame{data: annexB(5)}
	close(frames)

	var out bytes.Buffer
	p := Pipeline[testFrame]{Mode: ModeES, Split: splitTestFrame}
	if err := p.Run(nil, frames, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if payloads, _ := decodeMessages(t, out.Bytes()); len(payloads) != 1 {
		t.Fatalf("expected the empty frame to be skipped, got %d units", len(payloads))
	}
}

// TestPipelineStaleWatchdogFires is the regression test for the condition
// HCNetSDK reports by simply going quiet: no error, no channel close, just no
// more frames. Without the watchdog the pipeline would block here forever and
// the UI would hold a frozen last frame.
func TestPipelineStaleWatchdogFires(t *testing.T) {
	frames := make(chan testFrame) // never written to, never closed

	var out bytes.Buffer
	p := Pipeline[testFrame]{Mode: ModeES, Split: splitTestFrame, StaleTimeout: 20 * time.Millisecond}

	done := make(chan error, 1)
	go func() { done <- p.Run(nil, frames, &out) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrStale) {
			t.Fatalf("expected ErrStale, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return - the stale watchdog never fired")
	}
}

// TestPipelineClosedSourceEndsCleanly distinguishes the ordinary end-of-stream
// (ctx cancelled, Close called) from a stall - it must not be reported as an
// error, or every normal StopStream would surface a spurious stream:error.
func TestPipelineClosedSourceEndsCleanly(t *testing.T) {
	frames := make(chan testFrame)
	close(frames)

	var out bytes.Buffer
	p := Pipeline[testFrame]{Mode: ModeES, Split: splitTestFrame, StaleTimeout: time.Second}
	if err := p.Run(nil, frames, &out); err != nil {
		t.Fatalf("expected a closed source to end cleanly, got %v", err)
	}
}

// TestPipelinePSSkipsSysHead covers the one place ModePS treats a frame
// specially: NET_DVR_SYSHEAD is an out-of-band decoder-init blob, not part of
// the PS/PES byte stream, and feeding it to the demuxer corrupts the parse.
func TestPipelinePSSkipsSysHead(t *testing.T) {
	// Deliberately not valid PS data - the assertion is that nothing is emitted,
	// which is true whether the demuxer skips it or never sees it. What would
	// fail here is a pipeline that passed SysHead bytes through as a unit.
	frames := make(chan testFrame, 1)
	frames <- testFrame{data: []byte{0xDE, 0xAD, 0xBE, 0xEF}, sysHead: true}
	close(frames)

	var out bytes.Buffer
	p := Pipeline[testFrame]{Mode: ModePS, Split: splitTestFrame}
	if err := p.Run([]testFrame{{data: []byte{0xCA, 0xFE}, sysHead: true}}, frames, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if out.Len() != 0 {
		t.Fatalf("expected no units from SysHead-only input, got %d bytes", out.Len())
	}
}

// TestPipelineReportsWriteErrors guards that a failed write to the viewer
// propagates instead of being swallowed into an endless loop.
func TestPipelineReportsWriteErrors(t *testing.T) {
	frames := make(chan testFrame, 1)
	frames <- testFrame{data: annexB(5)}
	close(frames)

	sentinel := errors.New("viewer gone")
	p := Pipeline[testFrame]{Mode: ModeES, Split: splitTestFrame}
	err := p.Run(nil, frames, failingWriter{sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the write error to propagate, got %v", err)
	}
}

// TestPipelineOnUnitReportsStats checks the callback that replaced this
// pipeline's inline logging, since package main's diagnostics now depend on it.
func TestPipelineOnUnitReportsStats(t *testing.T) {
	frames := make(chan testFrame, 2)
	frames <- testFrame{data: annexB(1, 'd')}
	frames <- testFrame{data: annexB(5, 'k')}
	close(frames)

	var stats []UnitStat
	p := Pipeline[testFrame]{
		Mode:   ModeES,
		Split:  splitTestFrame,
		OnUnit: func(s UnitStat) { stats = append(stats, s) },
	}
	if err := p.Run(nil, frames, &bytes.Buffer{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(stats) != 2 {
		t.Fatalf("expected 2 OnUnit calls, got %d", len(stats))
	}
	if stats[0].Count != 1 || stats[1].Count != 2 {
		t.Errorf("Count should be 1-based and monotonic, got %d then %d", stats[0].Count, stats[1].Count)
	}
	if stats[0].Keyframe || !stats[1].Keyframe {
		t.Errorf("keyframe flags misreported: %v, %v", stats[0].Keyframe, stats[1].Keyframe)
	}
	if stats[0].SincePrevUnit != 0 {
		t.Errorf("the first unit has no predecessor, want SincePrevUnit 0, got %v", stats[0].SincePrevUnit)
	}
	if stats[1].Bytes != len(annexB(5, 'k')) {
		t.Errorf("Bytes = %d, want %d", stats[1].Bytes, len(annexB(5, 'k')))
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write(p []byte) (int, error) { return 0, f.err }

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
