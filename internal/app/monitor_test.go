package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMonitorHubFanOutAndUnsubscribe(t *testing.T) {
	hub := newMonitorHub()
	a := hub.subscribe()
	b := hub.subscribe()

	if !hub.hasSubscribers() {
		t.Fatal("hasSubscribers = false after subscribe")
	}

	hub.publish(MonitorEvent{Path: "/v1/chat/completions", Status: 200})
	for _, ch := range []chan MonitorEvent{a, b} {
		select {
		case ev := <-ch:
			if ev.Path != "/v1/chat/completions" {
				t.Fatalf("path = %q", ev.Path)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}

	hub.unsubscribe(a)
	hub.publish(MonitorEvent{Path: "/v1/models"})
	select {
	case _, ok := <-a:
		if ok {
			t.Fatal("unsubscribed channel received an event")
		}
	default:
	}
	select {
	case ev := <-b:
		if ev.Path != "/v1/models" {
			t.Fatalf("path = %q", ev.Path)
		}
	case <-time.After(time.Second):
		t.Fatal("remaining subscriber did not receive event")
	}

	hub.unsubscribe(b)
	if hub.hasSubscribers() {
		t.Fatal("hasSubscribers = true after all unsubscribed")
	}
}

func TestMonitorHubDropsWhenSubscriberBufferFull(t *testing.T) {
	hub := newMonitorHub()
	ch := hub.subscribe()
	// Buffer is 64; publishing many more must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			hub.publish(MonitorEvent{Status: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a full subscriber buffer")
	}
	if len(ch) != 64 {
		t.Fatalf("buffered = %d, want 64", len(ch))
	}
}

func TestMonitorMiddlewarePublishesEnrichedEvent(t *testing.T) {
	hub := newMonitorHub()
	ch := hub.subscribe()
	defer hub.unsubscribe(ch)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setMonitorModel(r.Context(), "claude-sonnet-5", true)
		setMonitorAccount(r.Context(), "work")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h := monitorMiddleware(hub)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	h.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case ev := <-ch:
		if ev.Model != "claude-sonnet-5" || ev.Account != "work" || !ev.Stream {
			t.Fatalf("enrichment lost: %+v", ev)
		}
		if ev.Status != http.StatusTooManyRequests {
			t.Fatalf("status = %d", ev.Status)
		}
		if ev.Method != http.MethodPost || ev.Path != "/v1/chat/completions" {
			t.Fatalf("method/path = %s %s", ev.Method, ev.Path)
		}
		if ev.ClientIP != "127.0.0.1" {
			t.Fatalf("client ip = %q", ev.ClientIP)
		}
	case <-time.After(time.Second):
		t.Fatal("no event published")
	}
}

// slowBody is an io.ReadCloser whose first Read blocks for d, then EOFs —
// standing in for a slow Command Code response body.
type slowBody struct {
	d    time.Duration
	done bool
}

func (s *slowBody) Read(p []byte) (int, error) {
	if s.done {
		return 0, io.EOF
	}
	time.Sleep(s.d)
	s.done = true
	return 0, io.EOF
}

func (s *slowBody) Close() error { return nil }

func TestMonitorMiddlewareDecomposesProxyAndUpstreamTime(t *testing.T) {
	hub := newMonitorHub()
	ch := hub.subscribe()
	defer hub.unsubscribe(ch)

	// Real wall-clock sleeps: the timing wrapper charges body-Read time to
	// upstream, and the middleware derives proxy as total - upstream.
	const readCost = 80 * time.Millisecond
	const proxyCost = 80 * time.Millisecond

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &http.Response{Body: &slowBody{d: readCost}}
		wrapUpstreamBody(r.Context(), resp)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		time.Sleep(proxyCost)
		w.WriteHeader(http.StatusOK)
	})
	h := monitorMiddleware(hub)(inner)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	select {
	case ev := <-ch:
		t.Logf("total=%d upstream=%d proxy=%d", ev.TotalUS, ev.UpstreamUS, ev.ProxyUS)
		// Sleep precision is coarse (notably on Windows); assert only that each
		// half landed on the right order of magnitude and that they sum to total.
		half := (40 * time.Millisecond).Microseconds()
		if ev.UpstreamUS < half {
			t.Fatalf("upstream_us = %d, expected the body-read sleep to dominate it", ev.UpstreamUS)
		}
		if ev.ProxyUS < half {
			t.Fatalf("proxy_us = %d, expected the post-read sleep to land here", ev.ProxyUS)
		}
		if got := ev.ProxyUS + ev.UpstreamUS; got < ev.TotalUS-2000 || got > ev.TotalUS+2000 {
			t.Fatalf("proxy_us + upstream_us = %d, total_us = %d", got, ev.TotalUS)
		}
	case <-time.After(time.Second):
		t.Fatal("no event published")
	}
}

func TestMonitorMiddlewareIgnoresNonModelPathsAndIdleHub(t *testing.T) {
	hub := newMonitorHub()

	// No subscribers: middleware must not wrap or attach a collector.
	called := false
	h := monitorMiddleware(hub)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if monitorFromContext(r.Context()) != nil {
			t.Fatal("collector attached with no subscribers")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !called {
		t.Fatal("inner handler not called")
	}

	// With a subscriber but a non-model path: still no event.
	ch := hub.subscribe()
	defer hub.unsubscribe(ch)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/usage", nil))
	select {
	case ev := <-ch:
		t.Fatalf("published event for non-model path: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleAdminMonitorStreamsLiveEvents(t *testing.T) {
	hub := newMonitorHub()
	srv := httptest.NewServer(handleAdminMonitor(hub))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	// Wait for the subscription to register, then publish.
	deadline := time.Now().Add(2 * time.Second)
	for !hub.hasSubscribers() {
		if time.Now().After(deadline) {
			t.Fatal("subscription never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	hub.publish(MonitorEvent{Path: "/v1/responses", Model: "m", Status: 200, TotalUS: 12000})

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev MonitorEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if ev.Path != "/v1/responses" || ev.Model != "m" || ev.TotalUS != 12000 {
			t.Fatalf("unexpected event: %+v", ev)
		}
		return
	}
	t.Fatalf("stream ended before an event arrived: %v", sc.Err())
}
