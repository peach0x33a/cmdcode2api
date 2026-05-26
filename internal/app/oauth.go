package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
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

// generateState 生成随机 state token 防 CSRF
func generateState() (string, error) {
	state, err := randomHex(32)
	if err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.URLEncoding.EncodeToString([]byte(state)), nil
}

// runOAuth 启动本地 HTTP server，打印授权链接，等待 CC 回调，返回 API Key。
func runOAuth() (string, error) {
	// 找一个可用端口
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", oauthPortStart))
	if err != nil {
		// 尝试下一个端口
		for port := oauthPortStart + 1; port < oauthPortStart+oauthPortRange; port++ {
			listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				break
			}
		}
		if err != nil {
			return "", fmt.Errorf("无法启动回调服务器: %w", err)
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
		if err := server.Serve(listener); err != http.ErrServerClosed {
			// server 被关闭是正常的
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	state, err := generateState()
	if err != nil {
		server.Close()
		return "", err
	}
	callbackURL := fmt.Sprintf("http://localhost:%d/callback", port)
	authURL := fmt.Sprintf("%s/studio/auth/cli?callback=%s&state=%s",
		studioBaseURL, callbackURL, state)

	// 写入文件便于后续读取（解决 background 模式下日志不可见的问题）
	if err := os.WriteFile(".oauth_state", []byte(state), 0600); err != nil {
		server.Close()
		return "", fmt.Errorf("write oauth state: %w", err)
	}
	if err := os.WriteFile(".oauth_url", []byte(authURL), 0600); err != nil {
		server.Close()
		return "", fmt.Errorf("write oauth url: %w", err)
	}

	log.Printf("等待 Command Code 授权...")
	log.Printf("授权页面: %s", authURL)
	log.Printf("State: %s", state)

	fmt.Println()
	fmt.Println("⏳ 等待授权...")
	fmt.Println("   授权链接：")
	fmt.Printf("   %s\n", authURL)
	fmt.Printf("   State: %s\n", state)
	fmt.Println()

	// 等待结果或错误
	select {
	case cb := <-resultCh:
		server.Close()
		if cb.State != state {
			return "", fmt.Errorf("state token 不匹配，可能被篡改")
		}
		log.Printf("✓ 授权成功 — 用户: %s, Key: %s", cb.UserName, cb.KeyName)
		return cb.APIKey, nil
	case err := <-errCh:
		server.Close()
		return "", err
	case <-time.After(oauthTimeout):
		server.Close()
		return "", fmt.Errorf("OAuth timed out after %s", oauthTimeout)
	}
}
