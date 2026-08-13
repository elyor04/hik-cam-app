package main

import "sync"

// broadcaster fans out each published wire-format unit (see stream.go's
// wireHeaderSize) to every currently-subscribed HTTP viewer.
//
// The obvious simpler design - a single io.Pipe shared between the demux
// pipeline and "the" viewer - is what the hik-cam-test reference app used,
// and it has two real problems this replaces:
//
//   - Only one viewer can ever read it, and its slot is released only once
//     the *next* camera frame arrives and a write to the departed viewer's
//     dead connection fails. That's bounded by the camera's frame interval,
//     not by how fast the viewer actually left, so a remount (React strict
//     mode double-mount, a page navigation and back) can find the stream
//     still "busy" by a reader that's already gone.
//   - Writing into a pipe nobody is reading blocks the demux goroutine
//     outright, which in turn stops draining the SDK's frame channel and
//     starts shedding frames process-wide.
//
// publish is therefore non-blocking and infallible: a unit reaches every
// subscriber whose channel has room right now and is silently dropped for
// one that's fallen behind. That's safe because the client-side WebCodecs
// decoder already only ever shows the newest frame (see
// frontend/src/components/LiveView.tsx's output callback) - a dropped delta
// unit is a momentary skip that self-heals on the next keyframe, not a
// correctness problem.
type broadcaster struct {
	mu     sync.Mutex
	subs   map[int]chan []byte
	next   int
	closed bool
	// lastKeyframe is the most recently published keyframe wire unit (nil
	// until the first one), used to reseed a newly subscribing viewer - see
	// subscribe. Safe to retain indefinitely: writeUnit (stream.go)
	// allocates a fresh slice per call and never mutates one it already
	// handed off, the same property publish relies on to give the identical
	// slice to every subscriber.
	lastKeyframe []byte
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[int]chan []byte)}
}

// subChanBuffer absorbs brief scheduling jitter in an actively-draining
// viewer goroutine (loopback, so normally drained in well under a frame
// interval) without needing to be anywhere near a full GOP - a dropped unit
// just waits for the next keyframe to resync, per this file's doc comment.
const subChanBuffer = 16

// subscribe registers a viewer and returns an id (for unsubscribe) plus the
// channel it will receive wire units on. The channel is closed - by
// unsubscribe, or by closeAll once the stream ends - to signal the viewer to
// stop, which the receiver picks up through Go's own `v, ok := <-ch` idiom.
//
// Two edge cases are handled here rather than at the call site:
//
//   - If closeAll already ran (a viewer's HTTP handler read cs.broadcast
//     just as the pipeline's cleanup nilled it out, then subscribed to this
//     now-retired instance a moment later), the returned channel is
//     pre-closed instead of registered. A broadcaster only ever closeAll's
//     once - a stream restart builds an entirely new one - so registering
//     here would leave the caller blocked forever on a channel nothing will
//     publish to or close.
//   - A new subscriber is immediately seeded with the most recent keyframe,
//     before joining the live fan-out. Without it, a viewer joining an
//     already-running broadcast (GetStreamInfo's catch-up path, or any
//     second concurrent viewer) receives delta frames first; WebCodecs'
//     VideoDecoder rejects every chunk until it has seen a "key" one, so the
//     view stays blank until the encoder's next keyframe - seconds, at a
//     typical GOP length.
func (b *broadcaster) subscribe() (int, <-chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan []byte, subChanBuffer)
	if b.closed {
		close(ch)
		return id, ch
	}
	if b.lastKeyframe != nil {
		ch <- b.lastKeyframe // buffered channel, freshly created - never blocks
	}
	b.subs[id] = ch
	return id, ch
}

// unsubscribe removes and closes id's channel. Safe to call more than once
// (e.g. from the viewer's own deferred cleanup racing closeAll) - a second
// call for an already-removed id is a no-op.
func (b *broadcaster) unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// closeAll disconnects every current subscriber, called once when the stream
// pipeline itself ends, so still-connected HTTP handlers return promptly
// instead of hanging on units that will never come.
func (b *broadcaster) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}

// publish delivers data to every current subscriber, dropping it (never
// blocking) for one that isn't keeping up. The caller never mutates or
// retains data afterward (writeUnit allocates a fresh slice per call), so
// handing the same slice to every subscriber - and caching it as
// lastKeyframe for subscribe's reseed - is safe.
func (b *broadcaster) publish(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(data) > 0 && data[0]&wireFlagKeyframe != 0 {
		b.lastKeyframe = data
	}
	for _, ch := range b.subs {
		select {
		case ch <- data:
		default:
		}
	}
}

// broadcastWriter adapts publish to the io.Writer runDemuxPipeline already
// writes wire units through, so the framing logic needs no knowledge of
// how many viewers exist. Write always succeeds - see publish.
type broadcastWriter struct{ b *broadcaster }

func (w broadcastWriter) Write(p []byte) (int, error) {
	w.b.publish(p)
	return len(p), nil
}
