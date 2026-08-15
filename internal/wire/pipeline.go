package wire

import (
	"errors"
	"io"
	"time"

	"hik-cam-app/internal/video/h264"
	"hik-cam-app/internal/video/psdemux"
)

// Mode identifies which of the two shapes HCNetSDK's RealPlay data can arrive
// in. The SDK wrapper's RealPlay hardcodes byProtoType=0 (private protocol), and
// which of these a given device actually sends back is not something this app
// controls - package main sniffs it and passes the answer in.
type Mode int

const (
	ModeUnknown Mode = iota
	// ModePS: NET_DVR_SYSHEAD (1) + NET_DVR_STREAMDATA (2) [+ AUDIOSTREAMDATA
	// (3)] together form a raw MPEG Program Stream - Hikvision's
	// private-protocol container, NOT a bare H.264/H.265 elementary stream.
	// Demuxed natively by internal/video/psdemux. This is what real hardware has
	// actually been observed to send.
	ModePS
	// ModeES: NET_DVR_STD_VIDEODATA (4) is a genuine bare elementary stream (no
	// container). Kept as a defensive fallback - each frame is assumed to
	// already be one complete Annex-B access unit.
	ModeES
)

// DefaultStaleTimeout bounds how long Run waits for the *next* frame once
// streaming is already under way before concluding the connection is dead (e.g.
// the network cable was pulled). This watchdog is not redundant with HCNetSDK's
// own exception callback (see app.go's handleSDKException): against real
// hardware, EXCEPTION_PREVIEW has been observed not to fire at all for minutes
// after an actual cable pull - the SDK's frame callback simply stops being
// invoked, with no error and no channel close. A camera at ~25fps sends a frame
// every ~40ms, so 5s is generous headroom while still being far faster than
// HCNetSDK's own 30s reconnect interval.
const DefaultStaleTimeout = 5 * time.Second

// ErrStale is returned by Run when no frame arrived before the stale-stream
// watchdog fired.
var ErrStale = errors.New("no frame received before the stale-stream watchdog fired (camera unreachable?)")

// UnitStat describes one access unit as it is written, for the caller's own
// logging. Keeping this a callback rather than logging here is what lets this
// package stay free of the SDK: package main's handler folds in
// hikvision.DroppedFrameCount and the source channel's backlog, neither of which
// is visible from in here.
type UnitStat struct {
	// Count is this unit's 1-based index within the pipeline run.
	Count int
	// Bytes is the access unit's payload length, excluding HeaderSize.
	Bytes int
	// Keyframe reports whether the unit contains an IDR slice.
	Keyframe bool
	// SincePrevUnit is the gap since the previous unit was written; zero for the
	// first.
	SincePrevUnit time.Duration
	// SincePrevKeyframe is the gap since the previous keyframe; meaningful only
	// when Keyframe is true, and zero for the first keyframe.
	SincePrevKeyframe time.Duration
	// WriteDur is how long dst.Write took for this unit.
	WriteDur time.Duration
}

// Pipeline turns a camera's raw frames into this app's wire format (see Encode)
// written to an io.Writer. No decoding and no container remux happen here or
// anywhere in this process - decoding happens in the browser via WebCodecs.
//
// F is whatever frame type the caller's source produces; Split is how this
// package reads one without knowing about it. That indirection is what keeps the
// SDK out of this package while costing nothing at runtime - there is no
// conversion goroutine and no per-frame copy, so the backlog the caller measures
// on its own channel stays the real one.
type Pipeline[F any] struct {
	// Mode selects PS demuxing versus passthrough; see Mode.
	Mode Mode
	// Split reports a frame's payload bytes and whether it is an out-of-band
	// NET_DVR_SYSHEAD blob (which carries no PS/PES stream bytes and must be
	// skipped in ModePS - see internal/video/psdemux's package doc).
	Split func(F) (data []byte, sysHead bool)
	// StaleTimeout defaults to DefaultStaleTimeout when zero.
	StaleTimeout time.Duration
	// OnUnit, when non-nil, is called once per written access unit.
	OnUnit func(UnitStat)
}

// Run consumes buffered (the frames the caller already read while sniffing the
// codec) and then frames, writing wire messages to dst until the source closes,
// the watchdog fires, or a write fails.
func (p Pipeline[F]) Run(buffered []F, frames <-chan F, dst io.Writer) error {
	staleTimeout := p.StaleTimeout
	if staleTimeout <= 0 {
		staleTimeout = DefaultStaleTimeout
	}

	var unitCount int
	var lastUnitAt, lastKeyframeAt time.Time

	writeUnit := func(data []byte, keyframe bool) error {
		if len(data) == 0 {
			return nil
		}
		msg := Encode(data, keyframe)
		writeStart := time.Now()
		_, err := dst.Write(msg)
		writeDur := time.Since(writeStart)

		unitCount++
		if p.OnUnit != nil {
			stat := UnitStat{Count: unitCount, Bytes: len(data), Keyframe: keyframe, WriteDur: writeDur}
			if !lastUnitAt.IsZero() {
				stat.SincePrevUnit = time.Since(lastUnitAt)
			}
			if keyframe && !lastKeyframeAt.IsZero() {
				stat.SincePrevKeyframe = time.Since(lastKeyframeAt)
			}
			p.OnUnit(stat)
		}
		lastUnitAt = time.Now()
		if keyframe {
			lastKeyframeAt = lastUnitAt
		}
		return err
	}

	if p.Mode == ModeES {
		for _, f := range buffered {
			data, _ := p.Split(f)
			if err := writeUnit(data, h264.IsKeyframe(data)); err != nil {
				return err
			}
		}
		for {
			f, ok, err := readFrame(frames, staleTimeout)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			data, _ := p.Split(f)
			if len(data) == 0 {
				continue
			}
			if err := writeUnit(data, h264.IsKeyframe(data)); err != nil {
				return err
			}
		}
	}

	dmx := psdemux.New()
	feed := func(data []byte) error {
		for _, u := range dmx.Feed(data) {
			if err := writeUnit(u.Data, u.Keyframe); err != nil {
				return err
			}
		}
		return nil
	}

	for _, f := range buffered {
		data, sysHead := p.Split(f)
		if sysHead {
			continue // out-of-band decoder-init blob, not part of the PS/PES byte stream
		}
		if err := feed(data); err != nil {
			return err
		}
	}
	for {
		f, ok, err := readFrame(frames, staleTimeout)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		data, sysHead := p.Split(f)
		if len(data) == 0 || sysHead {
			continue
		}
		if err := feed(data); err != nil {
			return err
		}
	}
	// Flush whatever access unit is still being reassembled - it only completes
	// once the *next* one starts, so without this the final frame is dropped.
	if u := dmx.Flush(); u != nil {
		return writeUnit(u.Data, u.Keyframe)
	}
	return nil
}

// readFrame waits for the next frame or reports ErrStale if none arrives within
// timeout. ok mirrors a channel receive's second value: false with err == nil
// means frames was closed, i.e. the stream ended normally (Close/ctx
// cancellation), not a stall.
func readFrame[F any](frames <-chan F, timeout time.Duration) (f F, ok bool, err error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case f, ok = <-frames:
		return f, ok, nil
	case <-timer.C:
		var zero F
		return zero, false, ErrStale
	}
}
