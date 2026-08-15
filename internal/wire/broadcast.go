package wire

import "sync"

// Broadcaster fans out each published wire message (see Encode) to every
// currently-subscribed viewer.
//
// The obvious simpler design - a single io.Pipe shared between the demux
// pipeline and "the" viewer - is what the hik-cam-test reference app used, and
// it has two real problems this replaces:
//
//   - Only one viewer can ever read it, and its slot is released only once the
//     *next* camera frame arrives and a write to the departed viewer's dead
//     connection fails. That's bounded by the camera's frame interval, not by
//     how fast the viewer actually left, so a remount (React strict mode double
//     mount, a page navigation and back) can find the stream still "busy" by a
//     reader that's already gone.
//   - Writing into a pipe nobody is reading blocks the demux goroutine
//     outright, which in turn stops draining the SDK's frame channel and starts
//     shedding frames process-wide.
//
// Publish is therefore non-blocking and infallible: a unit reaches every
// subscriber whose channel has room right now and is silently dropped for one
// that's fallen behind. That's safe because the client-side WebCodecs decoder
// already only ever shows the newest frame (see useCameraStream.ts's output
// callback) - a dropped delta unit is a momentary skip that self-heals on the
// next keyframe, not a correctness problem.
type Broadcaster struct {
	mu     sync.Mutex
	subs   map[int]chan []byte
	next   int
	closed bool
	// lastKeyframe is the most recently published keyframe message (nil until
	// the first one), used to reseed a newly subscribing viewer - see Subscribe.
	// Safe to retain indefinitely: Encode allocates a fresh slice per call and
	// nothing mutates one it already handed off, the same property Publish
	// relies on to give the identical slice to every subscriber.
	lastKeyframe []byte
}

// NewBroadcaster returns a Broadcaster ready to Subscribe to.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[int]chan []byte)}
}

// subChanBuffer absorbs brief scheduling jitter in an actively-draining viewer
// goroutine (loopback, so normally drained in well under a frame interval)
// without needing to be anywhere near a full GOP - a dropped unit just waits for
// the next keyframe to resync, per this type's doc comment.
const subChanBuffer = 16

// Subscribe registers a viewer and returns an id (for Unsubscribe) plus the
// channel it will receive wire messages on. The channel is closed - by
// Unsubscribe, or by CloseAll once the stream ends - to signal the viewer to
// stop, which the receiver picks up through Go's own `v, ok := <-ch` idiom.
//
// Two edge cases are handled here rather than at the call site:
//
//   - If CloseAll already ran (a viewer's HTTP handler read the session's
//     Broadcaster just as the pipeline's cleanup nilled it out, then subscribed
//     to this now-retired instance a moment later), the returned channel is
//     pre-closed instead of registered. A Broadcaster only ever CloseAll's once,
//     since a stream restart builds an entirely new one, so registering here
//     would leave the caller blocked forever on a channel nothing will publish
//     to or close.
//   - A new subscriber is immediately seeded with the most recent keyframe,
//     before joining the live fan-out. Without it, a viewer joining an
//     already-running broadcast (the frontend's catch-up path, or any second
//     concurrent viewer) receives delta frames first; WebCodecs' VideoDecoder
//     rejects every chunk until it has seen a "key" one, so the view stays blank
//     until the encoder's next keyframe - seconds, at a typical GOP length.
func (b *Broadcaster) Subscribe() (int, <-chan []byte) {
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

// Unsubscribe removes and closes id's channel. Safe to call more than once
// (e.g. from the viewer's own deferred cleanup racing CloseAll) - a second call
// for an already-removed id is a no-op.
func (b *Broadcaster) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// CloseAll disconnects every current subscriber, called once when the stream
// pipeline itself ends, so still-connected HTTP handlers return promptly instead
// of hanging on units that will never come.
func (b *Broadcaster) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}

// Publish delivers msg to every current subscriber, dropping it (never blocking)
// for one that isn't keeping up. The caller must not mutate or retain msg
// afterward - see Encode.
func (b *Broadcaster) Publish(msg []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if MessageIsKeyframe(msg) {
		b.lastKeyframe = msg
	}
	for _, ch := range b.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Writer adapts Publish to the io.Writer the demux pipeline writes units
// through, so the framing logic needs no knowledge of how many viewers exist.
// Write always succeeds - see Publish.
type Writer struct{ B *Broadcaster }

func (w Writer) Write(p []byte) (int, error) {
	w.B.Publish(p)
	return len(p), nil
}
