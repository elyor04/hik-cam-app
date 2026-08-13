package main

import (
	"context"
	"fmt"
	"log"
)

// StartANPR subscribes to the connected device's built-in license-plate
// recognition events; each detection is pushed to the frontend as an
// "anpr:event" Wails event carrying a PlateEventDTO.
//
// This is entirely independent of the live-view pipeline (stream.go): plates
// are recognized by the camera's own onboard ANPR and arrive as alarm
// uploads, not derived from decoded video frames. Streaming and ANPR can run
// in either order, or one without the other.
//
// go-hikvision-sdk's Device.PlateEvents keys its delivery channel by the
// device's userID (not per-call), so a second concurrent subscription on the
// same device would silently steal events from the first with no error - the
// anprCancel guard below is required, not just a courtesy check.
func (a *App) StartANPR() error {
	cs := &a.cam

	cs.mu.Lock()
	dev := cs.device
	if dev == nil {
		cs.mu.Unlock()
		return fmt.Errorf("not connected")
	}
	if cs.anprCancel != nil {
		cs.mu.Unlock()
		return fmt.Errorf("ANPR already active")
	}
	ctx, cancel := context.WithCancel(a.ctx)
	token := new(int) // see cameraSession.anprToken
	cs.anprCancel = cancel
	cs.anprToken = token
	cs.mu.Unlock()

	events, err := dev.PlateEvents(ctx)
	if err != nil {
		cancel()
		cs.mu.Lock()
		if cs.anprToken == token {
			cs.anprCancel, cs.anprToken = nil, nil
		}
		cs.mu.Unlock()
		log.Printf("[anpr] subscription failed: %v", err)
		return err
	}
	log.Printf("[anpr] subscribed")

	go func() {
		for ev := range events {
			log.Printf("[anpr] plate=%q confidence=%d%% speed=%dkm/h direction=%d lane=%d scene=%dB plate-img=%dB",
				ev.License, ev.Confidence, ev.SpeedKMH, ev.Direction, ev.Lane, len(ev.SceneImage), len(ev.PlateImage))
			a.emit(evtANPREvent, plateEventToDTO(ev))
		}
		cs.mu.Lock()
		// Only clear if the subscription is still ours. A fast
		// stop-then-start cycle can otherwise let this goroutine's belated
		// cleanup (events closes only once PlateEvents' own goroutines
		// unwind, which lags cancel() a little) null out a newer, still-live
		// subscription's cancel func - silently defeating both the "already
		// active" guard above and the double-subscription protection
		// PlateEvents needs. Mirrors runStreamPipeline comparing
		// cs.broadcast == bc before clearing it.
		if cs.anprToken == token {
			cs.anprCancel, cs.anprToken = nil, nil
		}
		cs.mu.Unlock()
		log.Printf("[anpr] subscription ended")
	}()
	return nil
}

// StopANPR unsubscribes from plate-detection events. Safe to call when
// nothing is subscribed.
func (a *App) StopANPR() error {
	cs := &a.cam

	cs.mu.Lock()
	cancel := cs.anprCancel
	cs.anprCancel, cs.anprToken = nil, nil
	cs.mu.Unlock()
	if cancel != nil {
		log.Printf("[anpr] StopANPR")
		cancel()
	}
	return nil
}

// IsANPRActive reports whether a plate-event subscription is currently live.
func (a *App) IsANPRActive() bool {
	a.cam.mu.Lock()
	defer a.cam.mu.Unlock()
	return a.cam.anprCancel != nil
}
