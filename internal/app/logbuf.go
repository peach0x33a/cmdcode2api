package app

import (
	"strings"
	"sync"
	"time"
)

// logRingCapacity bounds the in-memory log tail exposed to the WebUI.
const logRingCapacity = 500

type logEntry struct {
	Seq  int64     `json:"seq"`
	Time time.Time `json:"time"`
	Line string    `json:"line"`
}

// logRing keeps the most recent log lines in memory so the WebUI can show a
// live tail without filesystem access. It satisfies io.Writer so it can be
// attached to the stdlib logger.
type logRing struct {
	mu      sync.Mutex
	entries []logEntry // fixed-size ring, oldest slot overwritten
	next    int
	filled  int
	seq     int64
}

func newLogRing() *logRing {
	return &logRing{entries: make([]logEntry, logRingCapacity)}
}

func (r *logRing) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	line = stripANSI(line)
	if strings.TrimSpace(line) == "" {
		return len(p), nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.entries[r.next] = logEntry{Seq: r.seq, Time: time.Now(), Line: line}
	r.next = (r.next + 1) % len(r.entries)
	if r.filled < len(r.entries) {
		r.filled++
	}
	return len(p), nil
}

// After returns entries with a sequence number greater than seq, oldest
// first, plus the latest sequence number.
func (r *logRing) After(seq int64) ([]logEntry, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	last := r.seq
	out := make([]logEntry, 0, r.filled)
	for i := 0; i < r.filled; i++ {
		idx := (r.next - r.filled + i + len(r.entries)) % len(r.entries)
		if r.entries[idx].Seq > seq {
			out = append(out, r.entries[idx])
		}
	}
	return out, last
}

// stripANSI removes ANSI escape sequences so WebUI log lines render cleanly.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
