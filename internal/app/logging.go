package app

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
)

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

type flushStatusRecorder struct {
	*statusRecorder
	flusher http.Flusher
}

func newStatusRecorder(w http.ResponseWriter) (*statusRecorder, http.ResponseWriter) {
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return recorder, recorder
	}
	return recorder, &flushStatusRecorder{statusRecorder: recorder, flusher: flusher}
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *flushStatusRecorder) Flush() {
	r.flusher.Flush()
}

func formatHTTPLog(method, path string, status int, duration time.Duration, remoteAddr string) string {
	return fmt.Sprintf(
		"%s %s %s %s %s %s",
		colorize("[HTTP]", ansiDim),
		colorize(sanitizeLogField(method), methodColor(method)),
		sanitizeLogField(path),
		colorize(fmt.Sprintf("%d", status), statusColor(status)),
		colorize(formatDuration(duration), ansiDim),
		colorize(sanitizeLogField(clientIP(remoteAddr)), ansiDim),
	)
}

func sanitizeLogField(value string) string {
	if value == "" {
		return value
	}
	quoted := strconv.QuoteToASCII(value)
	return strings.Trim(quoted, "\"")
}

func formatDuration(duration time.Duration) string {
	if duration >= time.Second {
		return duration.Round(time.Millisecond).String()
	}
	return duration.Round(time.Microsecond).String()
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return host
	}
	return remoteAddr
}

// requestClientIP returns the client address for logging and admin rate
// limiting. Proxy headers are trusted only when the direct peer is a loopback
// address, which is the expected nginx-to-app deployment on one host.
func requestClientIP(r *http.Request) string {
	remote := clientIP(r.RemoteAddr)
	peer := net.ParseIP(remote)
	if peer == nil || !peer.IsLoopback() {
		return remote
	}

	if ip := parseHeaderIP(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if ip := firstForwardedIP(r.Header.Get("X-Forwarded-For")); ip != "" {
		return ip
	}
	if ip := parseHeaderIP(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	return remote
}

func parseHeaderIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if ip := net.ParseIP(value); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return ""
}

func firstForwardedIP(value string) string {
	for _, part := range strings.Split(value, ",") {
		if ip := parseHeaderIP(part); ip != "" {
			return ip
		}
	}
	return ""
}

func colorize(text, color string) string {
	if color == "" || !useColor() {
		return text
	}
	return color + text + ansiReset
}

func useColor() bool {
	_, noColor := os.LookupEnv("NO_COLOR")
	return !noColor
}

func statusColor(status int) string {
	switch {
	case status >= 200 && status < 300:
		return ansiGreen
	case status >= 300 && status < 400:
		return ansiCyan
	case status >= 400 && status < 500:
		return ansiYellow
	case status >= 500:
		return ansiRed
	default:
		return ""
	}
}

func methodColor(method string) string {
	switch method {
	case http.MethodGet:
		return ansiBlue
	case http.MethodPost:
		return ansiGreen
	case http.MethodPut, http.MethodPatch:
		return ansiYellow
	case http.MethodDelete:
		return ansiRed
	default:
		return ansiCyan
	}
}
