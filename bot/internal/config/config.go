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

	// How often the bot scans for new opportunities (minutes).
	ScanIntervalMin int

	// Minimum token price to enter a trade (0.93 = 93¢ favourite).
	EntryThreshold float64

	// Hard cap on position size regardless of Kelly output ($30).
	MaxPositionSize float64

	// Sports the bot is allowed to trade. Comma-separated.
	// e.g. "Baseball"  or  "Baseball,Soccer"
	Sports []string

	// Minimum time until market close (hours). Don't enter if game starts
	// in less than this many hours — avoids entering right before close.
	MinHoursToClose float64

	// Path to the shared SQLite database.
	DBPath string

	// Fallback position size when Kelly can't be computed (not enough
	// loss data yet). Kelly requires at least one loss to calculate b.
	FallbackSize float64
}

func Load() *Config {
	return &Config{
		DryRun:          envBool("DRY_RUN", true),
		ScanIntervalMin: envInt("SCAN_INTERVAL_MIN", 15),
		EntryThreshold:  envFloat("ENTRY_THRESHOLD", 0.93),
		MaxPositionSize: envFloat("MAX_POSITION_SIZE", 30.0),
		Sports:          envStrings("SPORTS", []string{"Baseball"}),
		MinHoursToClose: envFloat("MIN_HOURS_TO_CLOSE", 1.0),
		DBPath:          envString("DB_PATH", "../trades.db"),
		FallbackSize:    envFloat("FALLBACK_SIZE", 10.0),
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
