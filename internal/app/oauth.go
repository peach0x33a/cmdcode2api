package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	oauthPortStart = 5959
	oauthPortRange = 10
	studioBaseURL  = "https://commandcode.ai"
)

// oauthTimeout is a var (not const) so tests can temporarily lower it to
// exercise oauthListener.Wait's timeout branch without waiting out the real
// 10-minute default.
var oauthTimeout = 10 * time.Minute

type oauthCallback struct {
	APIKey   string `json:"apiKey"`
	State    string `json:"state"`
	UserID   string `json:"userId"`
	UserName string `json:"userName"`
	KeyName  string `json:"keyName"`
}

type OAuthOptions struct {
	CallbackURL string
}

// generateState generates a random state token to prevent CSRF.
func generateState() (string, error) {
	state, err := randomHex(32)
	if err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.URLEncoding.EncodeToString([]byte(state)), nil
}

// oauthListener is a started-but-not-yet-resolved local OAuth callback
// server: the port is bound, the auth URL is ready to show the user, and
// /callback is being served in the background. Wait blocks until it
// resolves. Splitting this out of runOAuth is what lets a session stay open
// across HTTP requests (see ReauthManager) instead of blocking one goroutine
// for the lifetime of the flow.
type oauthListener struct {
	AuthURL     string
	CallbackURL string
	State       string
	Port        int

	server   *http.Server
	resultCh chan oauthCallback
	errCh    chan error
}

// startOAuthListener finds an available port, starts a local HTTP server, and
// returns the authorization link; it does not block waiting for the
// callback — callers wait for the result via the returned value's Wait().
func startOAuthListener(opts OAuthOptions) (*oauthListener, error) {
	// Find an available port
	listenHost := "127.0.0.1"
	listenPort := oauthPortStart
	if opts.CallbackURL != "" {
		if err := validateCallbackURL(opts.CallbackURL); err != nil {
			return nil, err
		}
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", listenHost, listenPort))
	if err != nil {
		// Try the next port
		for port := oauthPortStart + 1; port < oauthPortStart+oauthPortRange; port++ {
			listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", listenHost, port))
			if err == nil {
				listenPort = port
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("failed to start callback server: %w", err)
		}
	}

	resultCh := make(chan oauthCallback, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// CORS
		w.Header().Set("Access-Control-Allow-Origin", "https://commandcode.ai")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")

		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}

		if r.Method != "POST" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(405)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "method not allowed",
			})
			return
		}

		var cb oauthCallback
		if err := json.NewDecoder(r.Body).Decode(&cb); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "invalid JSON",
			})
			return
		}

		// Error callback
		if errMsg, _ := r.URL.Query()["error"]; len(errMsg) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"success": true})
			errCh <- fmt.Errorf("authorization canceled: %s", errMsg[0])
			return
		}

		if cb.APIKey == "" || cb.State == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "missing required fields",
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"success": true})

		resultCh <- cb
	})

	server := &http.Server{Handler: mux}
	go func() {
		if err := server.Serve(listener); err != http.ErrServerClosed {
			// server being closed is expected
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	state, err := generateState()
	if err != nil {
		server.Close()
		return nil, err
	}
	callbackURL := opts.CallbackURL
	if callbackURL == "" {
		callbackURL = fmt.Sprintf("http://localhost:%d/callback", port)
	}
	authURL := fmt.Sprintf("%s/studio/auth/cli?callback=%s&state=%s",
		studioBaseURL, callbackURL, state)

	// Write to a file for later reading (works around logs being invisible in background mode)
	if err := os.WriteFile(".oauth_state", []byte(state), 0600); err != nil {
		server.Close()
		return nil, fmt.Errorf("write oauth state: %w", err)
	}
	if err := os.WriteFile(".oauth_url", []byte(authURL), 0600); err != nil {
		server.Close()
		return nil, fmt.Errorf("write oauth url: %w", err)
	}

	return &oauthListener{
		AuthURL:     authURL,
		CallbackURL: callbackURL,
		State:       state,
		Port:        port,
		server:      server,
		resultCh:    resultCh,
		errCh:       errCh,
	}, nil
}

// Wait blocks until the callback resolves, an error is signaled, or
// oauthTimeout elapses, closing the listener's server in every case. It
// discards the identity fields on the callback (UserID/UserName/KeyName) —
// callers that need those should use WaitCallback instead.
func (l *oauthListener) Wait() (string, error) {
	cb, err := l.WaitCallback()
	if err != nil {
		return "", err
	}
	return cb.APIKey, nil
}

// WaitCallback blocks until the callback resolves, an error is signaled, or
// oauthTimeout elapses, closing the listener's server in every case. Unlike
// Wait, it returns the full callback payload so callers can persist the
// identity fields (UserID/UserName/KeyName) alongside the API key.
func (l *oauthListener) WaitCallback() (oauthCallback, error) {
	select {
	case cb := <-l.resultCh:
		l.server.Close()
		if cb.State != l.State {
			return oauthCallback{}, fmt.Errorf("state token mismatch, possibly tampered")
		}
		log.Printf("✓ authorization succeeded — user: %s, key: %s", cb.UserName, cb.KeyName)
		return cb, nil
	case err := <-l.errCh:
		l.server.Close()
		return oauthCallback{}, err
	case <-time.After(oauthTimeout):
		l.server.Close()
		return oauthCallback{}, fmt.Errorf("OAuth timed out after %s", oauthTimeout)
	}
}

// runOAuth starts a local HTTP server, prints the authorization link, waits
// for the CC callback, and returns the API Key.
func runOAuth(opts OAuthOptions) (string, error) {
	cb, err := runOAuthWithCallback(opts)
	if err != nil {
		return "", err
	}
	return cb.APIKey, nil
}

// runOAuthWithCallback behaves like runOAuth but returns the full callback
// payload — including the identity fields (UserID/UserName/KeyName) that
// Wait/runOAuth discard — for callers that need to persist them alongside
// the API key (e.g. the --oauth CLI path and the reauth flow).
func runOAuthWithCallback(opts OAuthOptions) (oauthCallback, error) {
	l, err := startOAuthListener(opts)
	if err != nil {
		return oauthCallback{}, err
	}

	log.Printf("waiting for Command Code OAuth callback on http://127.0.0.1:%d/callback", l.Port)

	fmt.Printf(`Command Code OAuth

Open this URL in your browser:
  %s

Callback URL:
  %s

If this is running on a remote server, make sure that callback URL reaches:
  http://127.0.0.1:%d/callback

Waiting for authorization, timeout: %s

`, l.AuthURL, l.CallbackURL, l.Port, oauthTimeout)

	return l.WaitCallback()
}

func validateCallbackURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse oauth callback url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("oauth callback url must use http or https")
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("oauth callback url must include a host")
	}
	if !strings.HasSuffix(parsed.Path, "/callback") {
		return fmt.Errorf("oauth callback url path must end with /callback")
	}
	return nil
}
