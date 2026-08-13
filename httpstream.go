package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// startHTTP opens the loopback listener /stream is served from, for the
// lifetime of the process. Port 0 lets the OS pick a free port; the frontend
// never hardcodes it, it gets the full URL from "stream:ready"/GetStreamInfo.
func (a *App) startHTTP() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("stream listener: %w", err)
	}
	a.httpAddr = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", a.serveStream)
	a.httpSrv = &http.Server{Handler: mux}
	go func() {
		if err := a.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[http] stream server exited: %v", err)
		}
	}()
	log.Printf("[http] stream server listening on %s", a.httpAddr)
	return nil
}

// serveStream forwards the active demux pipeline's wire-framed access-unit
// stream (see stream.go's runDemuxPipeline) to the HTTP response as chunked
// transfer, for as long as the viewer stays connected. Any number of viewers
// may connect concurrently - in practice at most the one <canvas> player,
// plus briefly a departing one overlapping a fresh one across a remount.
func (a *App) serveStream(w http.ResponseWriter, r *http.Request) {
	cs := &a.cam

	cs.mu.Lock()
	bc := cs.broadcast
	start := cs.streamStartedAt
	cs.mu.Unlock()
	if bc == nil {
		http.Error(w, "no active stream", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// fetch() enforces CORS (unlike an <img src>, which never needed it) -
	// the frontend's origin differs from this listener's own 127.0.0.1:<port>
	// origin, so without this header the browser blocks reading
	// response.body entirely even though the request itself succeeds.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// Not a real container format - this app's own length-prefixed access
	// unit framing (see stream.go's wireHeaderSize), parsed directly by the
	// frontend's fetch()/ReadableStream reader before payloads go to
	// WebCodecs. octet-stream just tells the browser not to interpret it.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	id, ch := bc.subscribe()
	defer bc.unsubscribe(id)

	log.Printf("[http] /stream: viewer connected (%s) at t+%v since StartStream", r.RemoteAddr, elapsed(start))
	dst := &firstByteLogger{w: flushWriter{w, flusher}, label: "http-response(fetch body)", start: start}

	// Selecting on r.Context().Done() - rather than only writing until a
	// write fails - is what makes a departing viewer's slot free up
	// promptly: net/http cancels the request context as soon as it notices
	// the client disconnect, which this loop sees on its next iteration
	// (sub-millisecond on loopback). Discovering it by a failed write
	// instead would mean waiting out however long until the camera's next
	// frame happens to arrive.
	var n int64
	for {
		select {
		case <-r.Context().Done():
			log.Printf("[http] /stream: viewer disconnected (%s) after %d bytes, %v since StartStream", r.RemoteAddr, n, elapsed(start))
			return
		case data, ok := <-ch:
			if !ok { // the stream itself ended (broadcaster.closeAll)
				log.Printf("[http] /stream: stream ended for viewer (%s) after %d bytes, %v since StartStream", r.RemoteAddr, n, elapsed(start))
				return
			}
			written, err := dst.Write(data)
			n += int64(written)
			if err != nil {
				log.Printf("[http] /stream: write failed for viewer (%s) after %d bytes: %v", r.RemoteAddr, n, err)
				return
			}
		}
	}
}

// flushWriter flushes after every write so the browser's fetch() reader sees
// each access unit as soon as it's demuxed, rather than sitting in
// net/http's own output buffering.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// elapsed formats time.Since(start) at millisecond resolution - every timing
// log in this pipeline is relative to the same StartStream call, so the
// numbers line up into one timeline across goroutines (and, via
// LogFrontend, across the Go/JS boundary too).
func elapsed(start time.Time) time.Duration { return time.Since(start).Round(time.Millisecond) }

// firstByteLogger wraps a writer and logs, once, how long it took from
// pipeline start until the first byte reached this point.
type firstByteLogger struct {
	w     io.Writer
	label string
	start time.Time
	once  sync.Once
}

func (f *firstByteLogger) Write(p []byte) (int, error) {
	f.once.Do(func() {
		log.Printf("[stream] %s: first byte at t+%v", f.label, elapsed(f.start))
	})
	return f.w.Write(p)
}
