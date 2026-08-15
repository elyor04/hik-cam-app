package wire

import "testing"

// These are ported from tarozi-post-app's internal/camera/broadcast_test.go,
// which covers a Broadcaster functionally identical to this one. Both fixes
// under test were already present here - this file is the regression net they
// were missing, not the fix itself.

// TestBroadcasterSubscribeAfterCloseAllDoesNotHang covers the window the HTTP
// handler opens by design: it reads the session's Broadcaster, releases the
// lock, and only then calls Subscribe - so the stream pipeline's cleanup can nil
// that field and call CloseAll on this exact instance in between. Without the
// closed-check in Subscribe, that leaves a fresh, never-registered,
// never-closed channel the viewer's handler would block on forever.
func TestBroadcasterSubscribeAfterCloseAllDoesNotHang(t *testing.T) {
	b := NewBroadcaster()
	b.CloseAll()

	id, ch := b.Subscribe()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the channel to be closed (or empty+closed), got a value with ok=true")
		}
	default:
		t.Fatal("expected Subscribe after CloseAll to return an already-closed channel, but a receive would have blocked")
	}

	// Unsubscribe on an id that was never actually registered (see Subscribe's
	// own doc comment) must be a harmless no-op, not a double-close panic.
	b.Unsubscribe(id)
}

// TestBroadcasterSubscribeSeedsMostRecentKeyframe covers the reseed in
// Subscribe: a viewer joining an already-running broadcast mid-GOP would
// otherwise receive delta units first, and WebCodecs' VideoDecoder rejects every
// chunk until it has seen a "key" one - leaving the canvas blank until the
// encoder's next natural keyframe.
func TestBroadcasterSubscribeSeedsMostRecentKeyframe(t *testing.T) {
	b := NewBroadcaster()

	delta := Encode([]byte("del"), false)
	keyframe := Encode([]byte("key"), true)

	// No keyframe published yet - a subscriber shouldn't get anything pre-seeded.
	_, ch0 := b.Subscribe()
	select {
	case v := <-ch0:
		t.Fatalf("expected nothing pre-seeded before any keyframe was published, got %q", v)
	default:
	}

	b.Publish(delta)    // must not be cached as a keyframe
	b.Publish(keyframe) // cached
	b.Publish(delta)    // published after the keyframe - not what's under test here

	// A late subscriber joining now must receive the cached keyframe first,
	// before anything else.
	_, ch1 := b.Subscribe()
	select {
	case v := <-ch1:
		if string(v) != string(keyframe) {
			t.Fatalf("expected the cached keyframe to be seeded first, got %q", v)
		}
	default:
		t.Fatal("expected Subscribe to immediately seed the most recent keyframe, but a receive would have blocked")
	}
}

// TestBroadcasterSubscribeBeforeCloseAllStillWorks guards the ordinary case: a
// subscriber that registers before the stream ends receives units, then sees its
// channel closed once CloseAll runs.
func TestBroadcasterSubscribeBeforeCloseAllStillWorks(t *testing.T) {
	b := NewBroadcaster()
	_, ch := b.Subscribe()

	unit := Encode([]byte("unit-1"), false)
	b.Publish(unit)
	select {
	case data, ok := <-ch:
		if !ok || string(data) != string(unit) {
			t.Fatalf("expected to receive published data, got %q ok=%v", data, ok)
		}
	default:
		t.Fatal("expected the published unit to be immediately receivable")
	}

	b.CloseAll()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the channel to be closed after CloseAll")
		}
	default:
		t.Fatal("expected the channel to already be closed after CloseAll")
	}
}

// TestBroadcasterPublishDoesNotBlockOnAStalledSubscriber is the property the
// whole design exists for: a viewer that stops draining must never stall the
// demux goroutine, because that goroutine is also what drains the SDK's frame
// channel.
func TestBroadcasterPublishDoesNotBlockOnAStalledSubscriber(t *testing.T) {
	b := NewBroadcaster()
	_, ch := b.Subscribe()

	// Never read from ch. Publishing well past the channel's buffer must still
	// return promptly rather than deadlocking.
	for i := 0; i < subChanBuffer*4; i++ {
		b.Publish(Encode([]byte("unit"), false))
	}

	if got := len(ch); got != subChanBuffer {
		t.Fatalf("expected the subscriber channel to fill to %d and then drop, got %d", subChanBuffer, got)
	}
}

// TestBroadcasterUnsubscribeStopsDelivery guards that a departed viewer's slot
// is genuinely released rather than merely closed.
func TestBroadcasterUnsubscribeStopsDelivery(t *testing.T) {
	b := NewBroadcaster()
	id, ch := b.Subscribe()
	b.Unsubscribe(id)

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected the channel to be closed after Unsubscribe")
		}
	default:
		t.Fatal("expected the channel to already be closed after Unsubscribe")
	}

	// Publishing afterwards must not panic on the closed channel.
	b.Publish(Encode([]byte("after"), true))
}
