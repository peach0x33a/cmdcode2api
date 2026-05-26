package app

import (
	"flag"
	"fmt"
	"log"
	"os"
)

const configFile = "config.yaml"

func Run() {
	oauthMode := flag.Bool("oauth", false, "通过浏览器 OAuth 获取 Command Code API Key")
	oauthCallback := flag.String("oauth-callback", "", "OAuth callback URL，例如 http://server.example.com:5959/callback")
	host := flag.String("host", "", "HTTP listen host，例如 localhost 或 0.0.0.0")
	port := flag.Int("port", 0, "HTTP listen port")
	flag.Parse()

	cfgPath := findConfig()

	// --oauth 模式：浏览器登录获取 API Key
	if *oauthMode {
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
		if cfg == nil {
			// 没有配置，先生成一份
			cfg2, err := defaultConfig()
			if err != nil {
				log.Fatalf("create config: %v", err)
			}
			if err := saveConfig(cfgPath, &cfg2); err != nil {
				log.Fatalf("create config: %v", err)
			}
			cfg = &cfg2
		}

		apiKey, err := runOAuth(OAuthOptions{CallbackURL: *oauthCallback})
		if err != nil {
			log.Fatalf("OAuth 失败: %v", err)
		}

		cfg.CommandCode.APIKey = apiKey
		if err := saveConfig(cfgPath, cfg); err != nil {
			log.Fatalf("保存配置失败: %v", err)
		}

		fmt.Printf("\n✅ API Key 已写入 %s\n", cfgPath)
		fmt.Println("   现在可以直接运行 cmdcode2api 启动服务了。")
		return
	}

	// 正常模式
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 首次运行 — 生成配置
	if cfg == nil {
		cfg2, err := defaultConfig()
		if err != nil {
			log.Fatalf("create config: %v", err)
		}
		if err := saveConfig(cfgPath, &cfg2); err != nil {
			log.Fatalf("create config: %v", err)
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
		fmt.Println("Command Code API key is not configured.")
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

	cc := NewCCClient(cfg.CommandCode.APIKey, cfg.CommandCode.BaseURL)
	usage := loadUsage()

	if err := runServer(cc, cfg, usage); err != nil {
		log.Fatalf("server: %v", err)
	}
	usage.save()
}

func findConfig() string {
	return configFile
}
