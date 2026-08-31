package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// MonitorEvent is one model-API call as surfaced to the web UI's Monitoring
// tab. It carries only call metadata — never request or response bodies — so
// the tab can show traffic flowing through the gateway live without becoming
// a log of prompt content.
//
// The three timings decompose the wall-clock cost of the call, in
// microseconds:
//
//   - UpstreamUS: time the proxy spent talking to Command Code — the
//     outbound request/handshake (client.Send, summed across any failover
//     attempts) plus every Read blocked on the upstream response body.
//   - ProxyUS: everything else the gateway's own goroutine did — decoding
//     the client request, translating it, normalizing the upstream SSE
//     stream, tool-call repair, and writing the response to the client.
//     Derived as TotalUS - UpstreamUS.
//   - TotalUS: the whole handler, middleware to last byte written.
type MonitorEvent struct {
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Model      string    `json:"model,omitempty"`
	Account    string    `json:"account,omitempty"`
	Status     int       `json:"status"`
	Stream     bool      `json:"stream"`
	TotalUS    int64     `json:"total_us"`
	ProxyUS    int64     `json:"proxy_us"`
	UpstreamUS int64     `json:"upstream_us"`
	ClientIP   string    `json:"client_ip,omitempty"`
}

// monitorHub fans MonitorEvents out to every connected SSE subscriber (each
// open Monitoring tab). It keeps no backlog: a subscriber only ever sees
// calls that happen after it connects, which is exactly what the tab
// promises ("just ones made while the tab is open").
type monitorHub struct {
	mu   sync.Mutex
	subs map[chan MonitorEvent]struct{}
}

func newMonitorHub() *monitorHub {
	return &monitorHub{subs: make(map[chan MonitorEvent]struct{})}
}

func (h *monitorHub) subscribe() chan MonitorEvent {
	ch := make(chan MonitorEvent, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *monitorHub) unsubscribe(ch chan MonitorEvent) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// publish delivers ev to every subscriber, dropping it for any subscriber
// whose buffer is full rather than blocking the request path on a slow
// browser.
func (h *monitorHub) publish(ev MonitorEvent) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *monitorHub) hasSubscribers() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// monitorCollector is the per-request scratch space the model handlers fill
// in (model, account, stream) so monitorMiddleware can publish a complete
// MonitorEvent once the handler returns. The handler only sees status codes
// indirectly (via the wrapped ResponseWriter), so it never touches Status.
//
// upstream accumulates the time attributable to Command Code: sendWithFailover
// adds each client.Send call, and wrapUpstreamBody's reader adds every Read
// that blocked on the upstream response. Both writes happen only on the
// request's own goroutine (parseStreamEvents reads the body synchronously),
// so no lock is needed.
type monitorCollector struct {
	model    string
	account  string
	stream   bool
	upstream time.Duration
}

// addUpstream is a nil-safe accumulator so callers deep in the request path
// (sendWithFailover, the upstream body reader) don't each have to nil-check a
// missing collector.
func (c *monitorCollector) addUpstream(d time.Duration) {
	if c == nil {
		return
	}
	c.upstream += d
}

// timingReadCloser accumulates, into a monitorCollector, the wall time spent
// blocked in Read on the upstream response body — the dominant component of
// "time waiting on Command Code" for a streamed completion.
type timingReadCloser struct {
	inner io.ReadCloser
	coll  *monitorCollector
}

func (t *timingReadCloser) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := t.inner.Read(p)
	t.coll.addUpstream(time.Since(start))
	return n, err
}

func (t *timingReadCloser) Close() error { return t.inner.Close() }

// wrapUpstreamBody swaps resp.Body for one that charges its Read time to the
// request's monitorCollector. A no-op when the request isn't being monitored
// or there is no body, so callers can invoke it unconditionally.
func wrapUpstreamBody(ctx context.Context, resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	c := monitorFromContext(ctx)
	if c == nil {
		return
	}
	resp.Body = &timingReadCloser{inner: resp.Body, coll: c}
}

type monitorCtxKey struct{}

// monitorFromContext returns the collector monitorMiddleware attached, or nil
// when the request is not being monitored (no tab open). Callers must nil-check.
func monitorFromContext(ctx context.Context) *monitorCollector {
	c, _ := ctx.Value(monitorCtxKey{}).(*monitorCollector)
	return c
}

func setMonitorModel(ctx context.Context, model string, stream bool) {
	if c := monitorFromContext(ctx); c != nil {
		c.model = model
		c.stream = stream
	}
}

func setMonitorAccount(ctx context.Context, account string) {
	if c := monitorFromContext(ctx); c != nil {
		c.account = account
	}
}

// isModelAPIPath identifies the OpenAI-compatible model endpoints a
// connecting client calls — the traffic the Monitoring tab exists to show.
// The admin surface (/ui, /accounts*, /admin/*) is deliberately excluded.
// Keep this list in sync with the /v1/* routes registered in
// newHandlerWithPolicy (server.go); a new client-facing endpoint added there
// must be added here too or it won't appear in the tab.
func isModelAPIPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/responses", "/v1/models":
		return true
	}
	return false
}

// monitorMiddleware records one MonitorEvent per model-API request whenever
// at least one Monitoring tab is connected. When none is, it is a single map
// check and adds nothing to the request path.
func monitorMiddleware(hub *monitorHub) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isModelAPIPath(r.URL.Path) || !hub.hasSubscribers() {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec, wrapped := newStatusRecorder(w)
			coll := &monitorCollector{}
			ctx := context.WithValue(r.Context(), monitorCtxKey{}, coll)
			next.ServeHTTP(wrapped, r.WithContext(ctx))
			total := time.Since(start)
			upstream := coll.upstream
			// Clamp: scheduler jitter can make the summed Read time edge just
			// past the outer wall clock. Proxy time is the remainder.
			if upstream > total {
				upstream = total
			}
			hub.publish(MonitorEvent{
				Time:       start,
				Method:     r.Method,
				Path:       r.URL.Path,
				Model:      coll.model,
				Account:    coll.account,
				Status:     rec.status,
				Stream:     coll.stream,
				TotalUS:    total.Microseconds(),
				ProxyUS:    (total - upstream).Microseconds(),
				UpstreamUS: upstream.Microseconds(),
				ClientIP:   clientIP(r.RemoteAddr),
			})
		})
	}
}

// handleAdminMonitor streams MonitorEvents to the web UI's Monitoring tab as
// Server-Sent Events. It sends nothing on connect (no history) and a comment
// heartbeat every 25s so proxies and the browser keep the connection open.
// Reachable only via the same loopback+Host+Origin gate as the rest of
// /admin/* (see isAdminPath in ui.go).
func handleAdminMonitor(hub *monitorHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-ticker.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}
}
