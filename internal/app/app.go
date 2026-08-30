package app

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
)

// Version is the program version, formatted as vX.Y.Z.
const Version = "v0.1.0"

const configFile = "config.yaml"

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
			if err := writeConfigTemplate(cfgPath, &cfg2); err != nil {
				log.Fatalf("create config failed: %v", err)
			}
			cfg = &cfg2
		}

		apiKey, err := runOAuth(OAuthOptions{CallbackURL: *oauthCallback})
		if err != nil {
			log.Fatalf("OAuth failed: %v", err)
		}

		cfg.CommandCode.APIKey = apiKey
		if err := saveConfig(cfgPath, cfg); err != nil {
			log.Fatalf("save config failed: %v", err)
		}

		fmt.Printf("\n✅ API key written to %s\n", cfgPath)
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
		if err := writeConfigTemplate(cfgPath, &cfg2); err != nil {
			log.Fatalf("create config failed: %v", err)
		}
		fmt.Printf(`cmdcode2api initialized.

Created config: %s
Local client key: %s

Next:
  1. Run ./cmdcode2api --oauth to connect Command Code.
  2. Run ./cmdcode2api again to start the local OpenAI-compatible API.

Use the local client key above as the Bearer token for your OpenAI client.
`, cfgPath, cfg2.APIKey)
		os.Exit(0)
	}

	// 检查是否填了 CC API Key
	if cfg.CommandCode.APIKey == "" {
		fmt.Println("Command Code API key not configured.")
		fmt.Println("Run ./cmdcode2api --oauth, then start cmdcode2api again.")
		os.Exit(1)
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
	if cfg.CommandCode.BaseURL == "" {
		cfg.CommandCode.BaseURL = "https://api.commandcode.ai"
	}
	if *debug {
		cfg.Debug = true
		debugMode = true
	}

	cc := NewCCClient(cfg.CommandCode.APIKey, cfg.CommandCode.BaseURL)
	usage := loadUsage()

	FetchProviderModels(cfg.CommandCode.BaseURL, cfg.CommandCode.APIKey)

	if err := runServer(cc, cfg, usage); err != nil {
		log.Fatalf("server failed: %v", err)
	}
	if err := usage.save(); err != nil {
		log.Printf("save usage failed: %v", err)
	}
}

func findConfig() string {
	return configFile
}
