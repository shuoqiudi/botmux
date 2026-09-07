package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/skrashevich/botmux/internal/bot"
	"github.com/skrashevich/botmux/internal/bridge"
	"github.com/skrashevich/botmux/internal/gateway"
	"github.com/skrashevich/botmux/internal/proxy"
	"github.com/skrashevich/botmux/internal/server"
	"github.com/skrashevich/botmux/internal/store"
	verpkg "github.com/skrashevich/botmux/internal/version"
	"github.com/skrashevich/botmux/pkg/logbuf"
)

// Build-time variables injected via ldflags
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// telegramAPIURL is the base URL for Telegram Bot API requests.
// Override with -tg-api flag or TELEGRAM_API_URL env var for testing or local Bot API server.
var telegramAPIURL = "https://api.telegram.org"

// @title BotMux API
// @version 1.0
// @description Multi-bot Telegram manager with proxying, routing and LLM-based message dispatch.
// @contact.name BotMux
// @license.name MIT
// @host localhost:8080
// @BasePath /
// @securityDefinitions.apikey CookieAuth
// @in cookie
// @name botmux_session
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description API key authentication. Use "Bearer bmx_..." format.
func main() {
	token := flag.String("token", "", "Telegram bot token (optional if bots already exist in DB)")
	tokenFile := flag.String("token-file", "", "Read the Telegram bot token from a file")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "botdata.db", "SQLite database path")
	webhookURL := flag.String("webhook", "", "Set webhook URL for the CLI bot (requires -token)")
	tgAPI := flag.String("tg-api", "", "Custom Telegram API base URL (default: https://api.telegram.org)")
	redisAddr := flag.String("redis-addr", "", "Redis address for durable Gateway Streams (for example redis:6379)")
	redisPasswordFile := flag.String("redis-password-file", "", "Read the Gateway Redis password from a file")
	redisDB := flag.Int("redis-db", 0, "Redis database for durable Gateway Streams")
	demoMode := flag.Bool("demo", false, "Enable demo mode with separate database and seeded data")
	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("botmux %s (commit: %s, built: %s)\n", version, commit, buildDate)
		os.Exit(0)
	}

	resolvedToken, err := resolveTelegramToken(*token, *tokenFile, os.Getenv("TELEGRAM_BOT_TOKEN"))
	if err != nil {
		log.Fatalf("Failed to load Telegram bot token: %v", err)
	}
	*token = resolvedToken

	if *tgAPI == "" {
		*tgAPI = os.Getenv("TELEGRAM_API_URL")
	}
	if *tgAPI != "" {
		telegramAPIURL = strings.TrimRight(*tgAPI, "/")
		log.Printf("Using custom Telegram API: %s", telegramAPIURL)
	}

	if !*demoMode && os.Getenv("DEMO_MODE") == "true" {
		*demoMode = true
	}

	// Demo mode: separate database, fake Telegram API, seeded data
	if *demoMode {
		telegramAPIURL = "https://telegram-bot-api.exe.xyz"
		log.Printf("Demo mode enabled. Telegram API: %s", telegramAPIURL)
		log.Printf("Demo credentials are enabled")
		*dbPath = "demo.db"
	}

	// Set up log buffer to capture application logs for web UI
	logBuf := logbuf.New(1000)
	log.SetOutput(io.MultiWriter(os.Stderr, logBuf))

	st, err := store.NewStore(*dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer st.Close()

	if *demoMode {
		seedDemoData(st)
	}

	pm := proxy.NewManager(st, telegramAPIURL)
	var redisClient *redis.Client
	var inboundCancel context.CancelFunc
	if *redisAddr != "" {
		redisPassword := ""
		if *redisPasswordFile != "" {
			data, err := os.ReadFile(*redisPasswordFile)
			if err != nil {
				log.Fatalf("Failed to read Redis password file: %v", err)
			}
			redisPassword = strings.TrimSpace(string(data))
		}
		redisClient = redis.NewClient(&redis.Options{Addr: *redisAddr, Password: redisPassword, DB: *redisDB})
		queue := gateway.NewRedisInboundQueue(redisClient)
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := redisClient.Ping(checkCtx).Err()
		if err == nil {
			err = queue.VerifyDurability(checkCtx)
		}
		cancel()
		if err != nil {
			log.Fatalf("Gateway Redis is not durably available: %v", err)
		}
		inbound := gateway.NewInbound(st, queue, nil, gateway.InboundConfig{})
		pm.SetInboundGateway(inbound)
		var inboundCtx context.Context
		inboundCtx, inboundCancel = context.WithCancel(context.Background())
		go func() {
			for inboundCtx.Err() == nil {
				if err := inbound.Run(inboundCtx); err != nil && inboundCtx.Err() == nil {
					log.Printf("[gateway] inbound worker stopped; retrying")
				}
				select {
				case <-inboundCtx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}()
	}
	if redisClient != nil {
		defer redisClient.Close()
	}
	if inboundCancel != nil {
		defer inboundCancel()
	}
	srv := server.NewServer(st, pm)
	srv.DemoMode = *demoMode
	srv.LogBuf = logBuf
	srv.VersionChecker = verpkg.NewChecker(version, commit, buildDate)
	srv.TgAPIBaseURL = telegramAPIURL

	// Register CLI bot if token is provided
	if *token != "" {
		cliBot, err := bot.NewBot(*token, st, 0, telegramAPIURL)
		if err != nil {
			log.Fatalf("Failed to create bot: %v", err)
		}

		botID, err := st.RegisterCLIBot(*token, cliBot.GetBotInfo())
		if err != nil {
			log.Fatalf("Failed to register CLI bot: %v", err)
		}
		cliBot.SetBotID(botID)

		st.MigrateLegacyChats(botID)
		pm.RegisterManagedBot(botID, cliBot)
		srv.RegisterBot(botID, cliBot)

		if *webhookURL != "" {
			if err := cliBot.SetWebhook(*webhookURL); err != nil {
				log.Fatalf("Failed to set webhook: %v", err)
			}
			pm.SetWebhookMode(botID)
			srv.SetWebhookHandler("/tghook", pm.WebhookHandler(botID))
			log.Printf("CLI bot [%d] @%s: webhook mode at %s", botID, cliBot.GetBotInfo(), *webhookURL)
		} else {
			if err := pm.DeleteWebhook(*token); err != nil {
				log.Printf("Warning: could not delete webhook: %v", err)
			}
			log.Printf("CLI bot [%d] @%s: polling mode", botID, cliBot.GetBotInfo())
		}
	} else if !*demoMode {
		bots, _ := st.GetBotConfigs()
		if len(bots) == 0 {
			log.Fatal("No token provided and no bots in database. Use -token flag or TELEGRAM_BOT_TOKEN env var to add the first bot, or add one via the web UI.")
		}
		log.Printf("No token provided, using %d bot(s) from database", len(bots))
	}

	// Start ProxyManager for ALL bots
	pm.Start()
	defer pm.StopAll()

	// Start BridgeManager
	bridgeMgr := bridge.NewManager(st, pm, telegramAPIURL)
	bridgeMgr.Start()
	srv.SetBridgeManager(bridgeMgr)

	// Set bridge notification hook on all managed bots
	bridgeMgr.InstallHooks()

	// Graceful shutdown: SIGINT/SIGTERM triggers srv.Shutdown(15s).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.StartContext(ctx, *addr); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
	log.Printf("shutdown: complete")
}

func resolveTelegramToken(flagToken, tokenFile, environmentToken string) (string, error) {
	if flagToken != "" && tokenFile != "" {
		return "", fmt.Errorf("-token and -token-file cannot be used together")
	}
	if tokenFile == "" {
		if flagToken != "" {
			return flagToken, nil
		}
		return environmentToken, nil
	}

	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSuffix(string(data), "\n")
	token = strings.TrimSuffix(token, "\r")
	if token == "" {
		return "", fmt.Errorf("token file is empty")
	}
	if strings.ContainsAny(token, "\r\n") {
		return "", fmt.Errorf("token file must contain exactly one line")
	}
	return token, nil
}
