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
	"sync"
	"time"
)

const (
	oauthPortStart = 5959
	oauthPortRange = 10
	studioBaseURL  = "https://commandcode.ai"
	oauthTimeout   = 10 * time.Minute
)

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

// OAuthFlow is a running OAuth authorization. The CLI blocks on Wait; the
// WebUI polls State until it leaves "pending". All methods are safe for
// concurrent use.
type OAuthFlow struct {
	AuthURL     string
	CallbackURL string
	Port        int
	State       string

	server    *http.Server
	resultCh  chan oauthCallback
	errCh     chan error
	done      chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	result *oauthCallback
	err    error
}

// generateState 生成随机 state token 防 CSRF
func generateState() (string, error) {
	state, err := randomHex(32)
	if err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.URLEncoding.EncodeToString([]byte(state)), nil
}

// StartOAuthFlow starts the local callback server and returns a handle for
// awaiting the authorization result. The caller must eventually Wait or
// Cancel the flow so the listener is released.
func StartOAuthFlow(opts OAuthOptions) (*OAuthFlow, error) {
	listenHost := "127.0.0.1"
	listenPort := oauthPortStart
	if opts.CallbackURL != "" {
		if err := validateCallbackURL(opts.CallbackURL); err != nil {
			return nil, err
		}
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", listenHost, listenPort))
	if err != nil {
		// 尝试下一个端口
		for port := oauthPortStart + 1; port < oauthPortStart+oauthPortRange; port++ {
			listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", listenHost, port))
			if err == nil {
				listenPort = port
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("无法启动回调服务器: %w", err)
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

		// 错误回调
		if errMsg, _ := r.URL.Query()["error"]; len(errMsg) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"success": true})
			errCh <- fmt.Errorf("授权被取消: %s", errMsg[0])
			return
		}

		if cb.APIKey == "" || cb.State == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "缺少必要字段",
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
		_ = server.Serve(listener) // server 被关闭是正常的
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

	// 写入文件便于后续读取（解决 background 模式下日志不可见的问题）
	if err := os.WriteFile(".oauth_state", []byte(state), 0600); err != nil {
		server.Close()
		return nil, fmt.Errorf("write oauth state: %w", err)
	}
	if err := os.WriteFile(".oauth_url", []byte(authURL), 0600); err != nil {
		server.Close()
		return nil, fmt.Errorf("write oauth url: %w", err)
	}

	return &OAuthFlow{
		AuthURL:     authURL,
		CallbackURL: callbackURL,
		Port:        port,
		State:       state,
		server:      server,
		resultCh:    resultCh,
		errCh:       errCh,
		done:        make(chan struct{}),
	}, nil
}

// Wait blocks until the flow succeeds, fails, or times out.
func (f *OAuthFlow) Wait(timeout time.Duration) (oauthCallback, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case cb := <-f.resultCh:
		if cb.State != f.State {
			err := fmt.Errorf("state token 不匹配，可能被篡改")
			f.finish(err)
			return oauthCallback{}, err
		}
		f.mu.Lock()
		f.result = &cb
		f.mu.Unlock()
		f.finish(nil)
		return cb, nil
	case err := <-f.errCh:
		f.finish(err)
		return oauthCallback{}, err
	case <-timer.C:
		err := fmt.Errorf("OAuth timed out after %s", timeout)
		f.finish(err)
		return oauthCallback{}, err
	}
}

// Cancel aborts a pending flow. It is safe to call at any time.
func (f *OAuthFlow) Cancel() {
	select {
	case f.errCh <- fmt.Errorf("OAuth flow canceled"):
	default:
	}
	f.finish(nil)
}

// finish marks the flow complete exactly once and releases the listener.
func (f *OAuthFlow) finish(err error) {
	if err != nil {
		f.mu.Lock()
		if f.err == nil {
			f.err = err
		}
		f.mu.Unlock()
	}
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.server.Close()
	})
}

// State returns "pending", "success", or "failed" without blocking.
func (f *OAuthFlow) StateName() string {
	select {
	case <-f.done:
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.err != nil {
			return "failed"
		}
		return "success"
	default:
		return "pending"
	}
}

// Result returns the callback payload once the state is "success".
func (f *OAuthFlow) Result() (oauthCallback, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.result == nil {
		return oauthCallback{}, false
	}
	return *f.result, true
}

// Err returns the failure reason once the state is "failed".
func (f *OAuthFlow) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// runOAuth runs the CLI flow: start, print instructions, wait.
func runOAuth(opts OAuthOptions) (string, error) {
	flow, err := StartOAuthFlow(opts)
	if err != nil {
		return "", err
	}

	log.Printf("waiting for Command Code OAuth callback on http://127.0.0.1:%d/callback", flow.Port)

	fmt.Printf(`Command Code OAuth

Open this URL in your browser:
  %s

Callback URL:
  %s

If this is running on a remote server, make sure that callback URL reaches:
  http://127.0.0.1:%d/callback

Waiting for authorization, timeout: %s

`, flow.AuthURL, flow.CallbackURL, flow.Port, oauthTimeout)

	cb, err := flow.Wait(oauthTimeout)
	if err != nil {
		return "", err
	}
	log.Printf("✓ OAuth success: user %s, key %s", cb.UserName, cb.KeyName)
	return cb.APIKey, nil
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
