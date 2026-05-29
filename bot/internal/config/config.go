package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all tunable bot parameters.
// Every field is settable via environment variable so you can change
// thresholds without recompiling. Defaults match the agreed starting values.
type Config struct {
	// DryRun=true → paper trading (default). Set DRY_RUN=false to go live.
	DryRun bool

	// How often the bot runs a full market discovery scan to rebuild the
	// watchlist of active games (seconds). Default 600 = every 10 minutes.
	// This is the slow loop — it paginates all 157k markets.
	ScanIntervalSec int

	// How often the bot polls each watchlisted market for a qualifying price
	// (seconds). Default 10 = every 10 seconds.
	// This is the fast loop — single HTTP request per market.
	PollIntervalSec int

	// Minimum token price to enter a trade (global default across all sports).
	EntryThreshold float64

	// Maximum token price (global default) — avoids near-resolved markets where
	// fees exceed upside. At 0.97 the max gain is 3¢/$ risked; above that fees
	// dominate. Set 0 to disable.
	MaxEntryPrice float64

	// Per-sport price overrides. When set, these take precedence over the global
	// EntryThreshold / MaxEntryPrice for that specific sport.
	//
	// Tennis: edge only exists at 96¢+; at 94–95¢ the historical win rate (91%)
	// is below the market-implied probability.
	// TENNIS_MIN_PRICE (default 0.96), TENNIS_MAX_PRICE (default 0.97)
	TennisMinPrice float64
	TennisMaxPrice float64

	// Baseball: edge exists at 94–95.5¢; at 96¢+ favourites lose more often
	// than the market implies (MLB variance is higher than tennis).
	// BASEBALL_MIN_PRICE (default 0.94), BASEBALL_MAX_PRICE (default 0.955)
	BaseballMinPrice float64
	BaseballMaxPrice float64

	// Hockey: high-variance sport — starting conservatively at 95¢+ until
	// enough data is accumulated to tune the bounds.
	// HOCKEY_MIN_PRICE (default 0.95), HOCKEY_MAX_PRICE (default 0.97)
	HockeyMinPrice float64
	HockeyMaxPrice float64

	// Soccer (UCL, UEL, MLS, etc.): only trade in the final 30 minutes of a match
	// (≈70th minute onward). EndDateISO is used as a proxy for elapsed game time.
	// SOCCER_MIN_PRICE (default 0.94), SOCCER_MAX_PRICE (default 0.97)
	// SOCCER_MAX_HOURS_TO_CLOSE (default 0.5 = 30 min remaining ≈ 70th minute)
	SoccerMinPrice       float64
	SoccerMaxPrice       float64
	SoccerMaxHoursToClose float64

	// Hard cap on position size regardless of Kelly output ($30).
	MaxPositionSize float64

	// Minimum total market volume in USD before the bot will enter.
	// Filters out thin/illiquid markets. 0 = disabled.
	// MIN_VOLUME (default 50000)
	MinVolume float64

	// Sports the bot is allowed to trade. Comma-separated.
	// e.g. "Baseball"  or  "Baseball,Soccer"
	Sports []string

	// Minimum time until market close (hours). 0 = trade right up to the wire.
	MinHoursToClose float64

	// Maximum time until market close (hours). Skip markets closing further
	// away than this — avoids entering tomorrow's games or stale markets.
	MaxHoursToClose float64

	// Path to the shared SQLite database.
	DBPath string

	// Notification channels — leave empty to disable.
	DiscordWebhookURL string // DISCORD_WEBHOOK_URL
	TelegramBotToken  string // TELEGRAM_BOT_TOKEN
	TelegramChatID    string // TELEGRAM_CHAT_ID

	// Live trading credentials — only required when DRY_RUN=false.
	// Generated once via py-clob-client; stored as env vars on the server.
	PolyPrivateKey    string // POLY_PRIVATE_KEY    — EOA private key (hex, 0x-prefixed optional)
	PolyAPIKey        string // POLY_API_KEY         — L2 API key (UUID)
	PolyAPISecret     string // POLY_API_SECRET      — L2 API secret (base64url)
	PolyAPIPassphrase string // POLY_API_PASSPHRASE  — L2 passphrase (hex)
	// PolyProxyWallet is the Polymarket proxy wallet address that holds USDC.
	// Polymarket separates the signing key (EOA/MetaMask) from the funded wallet
	// (proxy). Orders must set maker=ProxyWallet, signer=EOA, signatureType=1.
	// Find it on polymarket.com → Profile — it differs from your MetaMask address.
	// POLY_PROXY_WALLET (required for live trading)
	PolyProxyWallet string

	// Fallback position size when Kelly can't be computed (not enough
	// loss data yet). Kelly requires at least one loss to calculate b.
	FallbackSize float64

	// ── Stop loss ─────────────────────────────────────────────────────────────

	// If a held token's price drops by this many cents from the entry price,
	// the bot paper-sells the position immediately rather than riding it to zero.
	// e.g. 0.50 = exit if price falls 50¢ from entry (96¢ entry → 46¢ stop).
	// Set to 0 to disable stop losses entirely.
	// STOP_LOSS_DROP (default 0.50)
	StopLossDrop float64

	// ── Safety nets ───────────────────────────────────────────────────────────

	// Maximum dollar loss allowed in a single calendar day.
	// Trading halts for the rest of the day once this is breached.
	// e.g. 300 = stop if today's resolved P&L drops below -$300.
	MaxDailyLoss float64

	// Number of consecutive losses that triggers a 24-hour trading pause.
	ConsecLossLimit int

	// Minimum tennis set required to enter. Live set state comes from the
	// sports_collector.py sidecar (live_sports table). When > 0, a tennis
	// market is only traded if the match is in this set or later — or in the
	// immediately prior set with someone serving for it (≥5 games). This
	// avoids early-match entries where a favorite still has lots of variance.
	//   0 = disabled (no set gating)
	//   3 = enter only during the 3rd set, or at the end of the 2nd
	// If enabled and no fresh live state is available, the tennis market is
	// skipped (fail-closed).
	// TENNIS_MIN_SET (default 0)
	TennisMinSet int

	// Bankroll floor expressed as a fraction of the current bankroll.
	// Bot shuts down entirely if balance falls below (bankroll × floor pct).
	// e.g. 0.30 = stop if balance drops to 30% of starting bankroll (70% loss).
	// Requires manual restart after investigating the cause.
	// BANKROLL_FLOOR_PCT (default 0.30)
	BankrollFloorPct float64
}

func Load() *Config {
	return &Config{
		DryRun:          envBool("DRY_RUN", true),
		ScanIntervalSec: envInt("SCAN_INTERVAL_SEC", 600),
		PollIntervalSec: envInt("POLL_INTERVAL_SEC", 10),
		EntryThreshold:  envFloat("ENTRY_THRESHOLD", 0.94),
		MaxEntryPrice:   envFloat("MAX_ENTRY_PRICE", 0.97),
		TennisMinPrice:  envFloat("TENNIS_MIN_PRICE", 0.96),
		TennisMaxPrice:  envFloat("TENNIS_MAX_PRICE", 0.97),
		BaseballMinPrice: envFloat("BASEBALL_MIN_PRICE", 0.94),
		BaseballMaxPrice: envFloat("BASEBALL_MAX_PRICE", 0.955),
		HockeyMinPrice:   envFloat("HOCKEY_MIN_PRICE", 0.95),
		HockeyMaxPrice:   envFloat("HOCKEY_MAX_PRICE", 0.97),
		SoccerMinPrice:       envFloat("SOCCER_MIN_PRICE", 0.94),
		SoccerMaxPrice:       envFloat("SOCCER_MAX_PRICE", 0.97),
		SoccerMaxHoursToClose: envFloat("SOCCER_MAX_HOURS_TO_CLOSE", 0.5),
		MaxPositionSize: envFloat("MAX_POSITION_SIZE", 30.0),
		MinVolume:       envFloat("MIN_VOLUME", 50000.0),
		Sports:          envStrings("SPORTS", []string{"Baseball", "Tennis"}),
		MinHoursToClose: envFloat("MIN_HOURS_TO_CLOSE", 0.0),
		MaxHoursToClose: envFloat("MAX_HOURS_TO_CLOSE", 0.0),
		DBPath:            envString("DB_PATH", "../trades.db"),
		DiscordWebhookURL: envString("DISCORD_WEBHOOK_URL", ""),
		TelegramBotToken:  envString("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:    envString("TELEGRAM_CHAT_ID", ""),
		PolyPrivateKey:    envString("POLY_PRIVATE_KEY", ""),
		PolyAPIKey:        envString("POLY_API_KEY", ""),
		PolyAPISecret:     envString("POLY_API_SECRET", ""),
		PolyAPIPassphrase: envString("POLY_API_PASSPHRASE", ""),
		PolyProxyWallet:   envString("POLY_PROXY_WALLET", ""),
		StopLossDrop:    envFloat("STOP_LOSS_DROP", 0.50),
		FallbackSize:    envFloat("FALLBACK_SIZE", 10.0),
		MaxDailyLoss:    envFloat("MAX_DAILY_LOSS", 300.0),
		ConsecLossLimit: envInt("CONSEC_LOSS_LIMIT", 3),
		TennisMinSet:    envInt("TENNIS_MIN_SET", 0),
		BankrollFloorPct: envFloat("BANKROLL_FLOOR_PCT", 0.30),
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return i
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envStrings(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return strings.Split(v, ",")
}
