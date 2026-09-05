package app

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
)

// Version is the program version, formatted as vX.Y.Z.
const Version = "v0.2.0"

// configFile is a var so tests can redirect config persistence to a temp dir.
var configFile = "config.yaml"

func Run() {
	oauthMode := flag.Bool("oauth", false, "通过浏览器 OAuth 获取 Command Code API Key")
	oauthCallback := flag.String("oauth-callback", "", "OAuth callback URL，例如 http://server.example.com:5959/callback")
	host := flag.String("host", "", "HTTP listen host，例如 localhost 或 0.0.0.0")
	port := flag.Int("port", 0, "HTTP listen port")
	debug := flag.Bool("debug", false, "print request body and all CC SSE events to stderr")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *version {
		fmt.Printf("cmdcode2api %s (go %s)\n", Version, runtime.Version())
		os.Exit(0)
	}

	cfgPath := findConfig()

	// --oauth 模式：浏览器登录获取 API Key
	if *oauthMode {
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			log.Fatalf("load config failed: %v", err)
		}
		if cfg == nil {
			// 没有配置，先生成一份
			cfg2, err := defaultConfig()
			if err != nil {
				log.Fatalf("create config failed: %v", err)
			}
			if err := writeConfigTemplate(cfgPath, cfg2); err != nil {
				log.Fatalf("create config failed: %v", err)
			}
			cfg = cfg2
		}

		apiKey, err := runOAuth(OAuthOptions{CallbackURL: *oauthCallback})
		if err != nil {
			log.Fatalf("OAuth failed: %v", err)
		}

		// OAuth 追加账号而不是覆盖：重复执行即可接入多个账号。
		pool := NewAccountPool(cfg.CommandCode.Accounts)
		if acct := pool.Get(accountID(apiKey)); acct != nil {
			fmt.Printf("\nℹ️  API key already configured as account %q in %s\n", acct.Name, cfgPath)
			return
		}
		name := oauthAccountName(pool)
		if _, err := pool.Add(name, apiKey, true); err != nil {
			log.Fatalf("add oauth account failed: %v", err)
		}
		pool.SyncToConfig(cfg)
		if err := saveConfig(cfgPath, cfg); err != nil {
			log.Fatalf("save config failed: %v", err)
		}

		fmt.Printf("\n✅ API key added as account %q in %s (%d account(s) total)\n", name, cfgPath, pool.Len())
		fmt.Println("   You can now run cmdcode2api to start the server.")
		return
	}

	// 正常模式
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	// 首次运行 — 生成配置
	if cfg == nil {
		cfg2, err := defaultConfig()
		if err != nil {
			log.Fatalf("create config failed: %v", err)
		}
		if err := writeConfigTemplate(cfgPath, cfg2); err != nil {
			log.Fatalf("create config failed: %v", err)
		}
		fmt.Printf(`cmdcode2api initialized.

Created config: %s
Local client key: %s
WebUI admin password: %s

Next:
  1. Run ./cmdcode2api --oauth to connect Command Code.
  2. Run ./cmdcode2api again to start the local OpenAI-compatible API.

Use the local client key above as the Bearer token for your OpenAI client.
The admin password above logs you into the WebUI at http://%s:%d/.
`, cfgPath, cfg2.APIKey, cfg2.AdminPassword, cfg2.Host, cfg2.Port)
		os.Exit(0)
	}

	// 检查是否配置了 CC 账号
	if len(cfg.CommandCode.Accounts) == 0 {
		fmt.Println("Command Code API key not configured.")
		fmt.Println("Run ./cmdcode2api --oauth, then start cmdcode2api again.")
		os.Exit(1)
	}

	// WebUI 管理密码为空时生成一个，只打印一次
	adminPasswordGenerated := false
	if cfg.adminPassword() == "" {
		password, err := genAdminPassword()
		if err != nil {
			log.Fatalf("generate admin password failed: %v", err)
		}
		cfg.setAdminPassword(password)
		if err := saveConfig(cfgPath, cfg); err != nil {
			log.Fatalf("save config failed: %v", err)
		}
		adminPasswordGenerated = true
	}

	if cfg.Port == 0 {
		cfg.Port = 11434
	}
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	if *host != "" {
		cfg.Host = *host
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if cfg.UpstreamBaseURL() == "" {
		cfg.SetUpstreamBaseURL("https://api.commandcode.ai")
	}
	if *debug {
		cfg.Debug = true
		debugMode = true
	}

	// 日志同时写入环形缓冲，供 WebUI 查看
	ring := newLogRing()
	log.SetOutput(io.MultiWriter(os.Stderr, ring))

	pool := NewAccountPool(cfg.CommandCode.Accounts)
	cc := NewCCClientWithPool(pool, cfg.UpstreamBaseURL())
	usage := loadUsage()

	if primary := pool.Primary(); primary != nil {
		FetchProviderModels(cfg.UpstreamBaseURL(), primary.APIKey)
	} else {
		log.Printf("[WARN] no enabled Command Code accounts; starting with an empty model catalog")
	}

	log.Printf("accounts: %d configured, %d enabled", pool.Len(), pool.EnabledCount())
	if adminPasswordGenerated {
		fmt.Printf("WebUI admin password generated: %s\n", cfg.adminPassword())
	}

	if err := runServer(cc, cfg, usage, ring); err != nil {
		log.Fatalf("server failed: %v", err)
	}
	if err := usage.save(); err != nil {
		log.Printf("save usage failed: %v", err)
	}
}

// oauthAccountName derives a label for an OAuth-added account.
func oauthAccountName(pool *AccountPool) string {
	return fmt.Sprintf("oauth-%d", pool.Len()+1)
}

func findConfig() string {
	return configFile
}
