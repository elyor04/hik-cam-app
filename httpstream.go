package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// streamPathToken is the unguessable path segment /stream is served under, e.g.
// /stream/9f3c1a.... It is what actually restricts who can read the camera feed.
//
// The loopback listener is reachable by more than this app: any page in any
// browser on this machine can fetch http://127.0.0.1:<port>/..., and the CORS
// header below is a permissive "*" because the frontend's own origin differs
// between `wails dev` and a packaged build, so pinning it to one literal origin
// would silently break playback in the other. A random path is the mitigation
// that does not depend on getting an origin string right: the URL is only ever
// handed to the frontend over the Wails bridge ("stream:ready"/GetStreamInfo),
// never guessable from outside, and never logged in full.
//
// That last property spans both languages and is enforced on the other side of
// the bridge: this file never logs the token, but the frontend receives the full
// URL and forwards its diagnostics here through LogFrontend, so the frontend is
// where it could leak into this process's own stdout. useCameraStream.ts's
// redactStreamToken strips it from every line before that happens. Keep the two
// together - a "never logged" claim only held up by one end is not held up.
//
// Generated once per process rather than per stream, so the URL stays stable
// across a stop/start cycle - a viewer only ever learns it through the bridge
// anyway, and rotating it would invalidate a URL a live panel is still holding.
func newStreamPathToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("stream path token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// startHTTP opens the loopback listener /stream/<token> is served from, for the
// lifetime of the process. Port 0 lets the OS pick a free port; the frontend
// never hardcodes either the port or the token, it gets the full URL from
// "stream:ready"/GetStreamInfo.
func (a *App) startHTTP() error {
	token, err := newStreamPathToken()
	if err != nil {
		return err
	}
	a.streamToken = token

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("stream listener: %w", err)
	}
	a.httpAddr = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/stream/"+token, a.serveStream)
	a.httpSrv = &http.Server{
		Handler: mux,
		// Bounds how long a connection may take to send its request headers.
		// WriteTimeout and ReadTimeout are deliberately left unset: this is an
		// open-ended streaming response, and either one would sever a healthy
		// live view at the deadline.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := a.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[http] stream server exited: %v", err)
		}
	}()
	// The token is deliberately not logged - it is the access control.
	log.Printf("[http] stream server listening on %s", a.httpAddr)
	return nil
}

// streamURL is the full URL a viewer should fetch the live stream from.
func (a *App) streamURL() string {
	return fmt.Sprintf("http://%s/stream/%s", a.httpAddr, a.streamToken)
}

// serveStream forwards the active demux pipeline's wire-framed access-unit
// stream (see internal/wire) to the HTTP response as chunked transfer, for as
// long as the viewer stays connected. Any number of viewers may connect
// concurrently - in practice at most the one <canvas> player, plus briefly a
// departing one overlapping a fresh one across a remount.
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

	// fetch() enforces CORS (unlike an <img src>, which never needed it) - the
	// frontend's origin differs from this listener's own 127.0.0.1:<port>
	// origin, so without this header the browser blocks reading response.body
	// entirely even though the request itself succeeds. See newStreamPathToken
	// for why this stays "*" and what actually gates access instead.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// Not a real container format - this app's own length-prefixed access unit
	// framing (see internal/wire's Encode), parsed directly by the frontend's
	// fetch()/ReadableStream reader before payloads go to WebCodecs.
	// octet-stream just tells the browser not to interpret it.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	id, ch := bc.Subscribe()
	defer bc.Unsubscribe(id)

	log.Printf("[http] /stream: viewer connected (%s) at t+%v since StartStream", r.RemoteAddr, elapsed(start))
	dst := &firstByteLogger{w: flushWriter{w, flusher}, label: "http-response(fetch body)", start: start}

	// Selecting on r.Context().Done() - rather than only writing until a write
	// fails - is what makes a departing viewer's slot free up promptly: net/http
	// cancels the request context as soon as it notices the client disconnect,
	// which this loop sees on its next iteration (sub-millisecond on loopback).
	// Discovering it by a failed write instead would mean waiting out however
	// long until the camera's next frame happens to arrive.
	var n int64
	for {
		select {
		case <-r.Context().Done():
			log.Printf("[http] /stream: viewer disconnected (%s) after %d bytes, %v since StartStream", r.RemoteAddr, n, elapsed(start))
			return
		case data, ok := <-ch:
			if !ok { // the stream itself ended (Broadcaster.CloseAll)
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
// each access unit as soon as it's demuxed, rather than sitting in net/http's
// own output buffering.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// elapsed formats time.Since(start) at millisecond resolution - every timing log
// in this pipeline is relative to the same StartStream call, so the numbers line
// up into one timeline across goroutines (and, via LogFrontend, across the Go/JS
// boundary too).
func elapsed(start time.Time) time.Duration { return time.Since(start).Round(time.Millisecond) }

// firstByteLogger wraps a writer and logs, once, how long it took from pipeline
// start until the first byte reached this point.
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
