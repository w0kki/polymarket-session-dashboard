package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/w0kki/polymarket-bot/internal/config"
	"github.com/w0kki/polymarket-bot/internal/db"
	"github.com/w0kki/polymarket-bot/internal/executor"
	"github.com/w0kki/polymarket-bot/internal/kelly"
	"github.com/w0kki/polymarket-bot/internal/market"
	"github.com/w0kki/polymarket-bot/internal/notify"
)

// watchlist holds markets that passed all structural filters. Rebuilt by the
// slow discovery loop; read by the fast poll loop.
var (
	watchlistMu sync.RWMutex
	watchlist   []market.WatchlistEntry
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmsgprefix)
	log.SetPrefix("")

	cfg := config.Load()

	mode := "PAPER"
	if !cfg.DryRun {
		mode = "LIVE"
	}
	log.Printf("══════════════════════════════════════════════")
	log.Printf("  Polymarket Risk Premia Bot — %s MODE", mode)
	log.Printf("  Sports:    %v", cfg.Sports)
	log.Printf("  Threshold: %.0f¢  Cap: $%.0f", cfg.EntryThreshold*100, cfg.MaxPositionSize)
	log.Printf("  Discovery: every %ds  Poll: every %ds", cfg.ScanIntervalSec, cfg.PollIntervalSec)
	log.Printf("  DB:        %s", cfg.DBPath)
	log.Printf("══════════════════════════════════════════════")

	// ── Notifier ─────────────────────────────────────────────────────────────
	notifier := notify.New(cfg.DiscordWebhookURL, cfg.TelegramBotToken, cfg.TelegramChatID)
	if notifier.Enabled() {
		channels := ""
		if cfg.DiscordWebhookURL != "" {
			channels += "Discord "
		}
		if cfg.TelegramBotToken != "" && cfg.TelegramChatID != "" {
			channels += "Telegram"
		}
		log.Printf("  Notifications: %s✓", channels)
	} else {
		log.Printf("  Notifications: disabled")
	}

	// ── Database ──────────────────────────────────────────────────────────────
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("cannot open database: %v", err)
	}
	defer database.Close()

	// ── Executor ──────────────────────────────────────────────────────────────
	var exec executor.Executor
	if cfg.DryRun {
		exec = executor.NewPaper(database)
	} else {
		exec = executor.NewLive()
	}

	// ── Scanner ───────────────────────────────────────────────────────────────
	sportBounds := map[string]market.SportBounds{
		"Tennis": {
			MinPrice: cfg.TennisMinPrice,
			MaxPrice: cfg.TennisMaxPrice,
		},
		"Baseball": {
			MinPrice: cfg.BaseballMinPrice,
			MaxPrice: cfg.BaseballMaxPrice,
		},
		"Hockey": {
			MinPrice: cfg.HockeyMinPrice,
			MaxPrice: cfg.HockeyMaxPrice,
		},
	}
	log.Printf("  Tennis: %.0f¢–%.0f¢  Baseball: %.0f¢–%.0f¢  Hockey: %.0f¢–%.0f¢",
		cfg.TennisMinPrice*100, cfg.TennisMaxPrice*100,
		cfg.BaseballMinPrice*100, cfg.BaseballMaxPrice*100,
		cfg.HockeyMinPrice*100, cfg.HockeyMaxPrice*100,
	)
	if cfg.MinVolume > 0 {
		log.Printf("  Min volume: $%.0f", cfg.MinVolume)
	}

	scanner := market.NewScanner(
		cfg.EntryThreshold,
		cfg.MaxEntryPrice,
		cfg.Sports,
		cfg.MaxPositionSize,
		cfg.MinHoursToClose,
		cfg.MaxHoursToClose,
		cfg.MinVolume,
		sportBounds,
	)

	// ── Signal handling ───────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Two-speed loop ────────────────────────────────────────────────────────
	// 1. Discovery (slow): full market scan → rebuilds watchlist + resolves trades
	// 2. Poll (fast): checks each watchlisted market for a qualifying price
	//
	// Discovery runs first so the watchlist is populated before polling starts.
	log.Printf("[discovery] initial scan starting...")
	runDiscovery(ctx, cfg, database, scanner, notifier)

	discoveryTicker := time.NewTicker(time.Duration(cfg.ScanIntervalSec) * time.Second)
	pollTicker := time.NewTicker(time.Duration(cfg.PollIntervalSec) * time.Second)
	defer discoveryTicker.Stop()
	defer pollTicker.Stop()

	for {
		select {
		case <-discoveryTicker.C:
			// Run in a goroutine so it doesn't block the poll ticker.
			go runDiscovery(ctx, cfg, database, scanner, notifier)
		case <-pollTicker.C:
			runPoll(ctx, cfg, database, scanner, exec, notifier)
		case <-ctx.Done():
			log.Println("shutting down")
			return
		}
	}
}

// ── Discovery loop ────────────────────────────────────────────────────────────

// runDiscovery does a full market scan, resolves open trades, and rebuilds
// the watchlist. Runs every ~10 minutes (SCAN_INTERVAL_SEC).
func runDiscovery(ctx context.Context, cfg *config.Config, database *db.DB, scanner *market.Scanner, n *notify.Notifier) {
	log.Println("── discovery start ──")

	// Resolve any open paper trades first.
	resolveOpenTrades(ctx, database, scanner, n)

	// Build dedup sets for watchlist filtering.
	traded, err := database.ActiveConditionIDs()
	if err != nil {
		log.Printf("[discovery] active positions error: %v", err)
		traded = map[string]bool{}
	}

	// Full market scan — no price filtering, just structural filters.
	entries, err := scanner.BuildWatchlist(traded, traded)
	if err != nil {
		log.Printf("[discovery] watchlist build error: %v", err)
		log.Println("── discovery end (error) ──")
		return
	}

	watchlistMu.Lock()
	watchlist = entries
	watchlistMu.Unlock()

	log.Printf("[discovery] watchlist: %d markets ready to poll", len(entries))
	log.Println("── discovery end ──")
}

// ── Poll loop ─────────────────────────────────────────────────────────────────

// runPoll checks every market on the watchlist for a qualifying price and
// executes trades when the threshold is crossed. Runs every 10s (POLL_INTERVAL_SEC).
func runPoll(ctx context.Context, cfg *config.Config, database *db.DB, scanner *market.Scanner, exec executor.Executor, n *notify.Notifier) {
	// Snapshot the watchlist under read lock.
	watchlistMu.RLock()
	entries := make([]market.WatchlistEntry, len(watchlist))
	copy(entries, watchlist)
	watchlistMu.RUnlock()

	if len(entries) == 0 {
		return // discovery hasn't run yet
	}

	// Load bankroll and compute Kelly sizing.
	bankroll, err := database.GetBankroll()
	if err != nil || bankroll <= 0 {
		bankroll = cfg.FallbackSize * 3
	}

	stats, err := database.GetTradeStats()
	if err != nil {
		stats = &db.TradeStats{}
	}
	kellyResult := kelly.Compute(
		stats.Wins, stats.Losses,
		stats.AvgWin, stats.AvgLoss,
		bankroll, cfg.MaxPositionSize, cfg.FallbackSize,
	)
	sizer := func(_ float64) float64 { return kellyResult.PositionSize }

	// Check safety nets before entering any new positions.
	if checkSafetyNets(cfg, database, bankroll, n) {
		return
	}

	// Poll each watchlisted market.
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Dedup: skip if already traded (catches trades placed since last discovery).
		already, err := database.IsAlreadyTraded(entry.ConditionID)
		if err != nil {
			log.Printf("[poll] dedup error for %s: %v", entry.ConditionID[:12], err)
			continue
		}
		if already {
			continue
		}

		opp, err := scanner.PollOpportunity(entry, sizer)
		if err != nil {
			// Log quietly — poll errors are expected for closed/expired markets.
			log.Printf("[poll] %s: %v", entry.ConditionID[:12], err)
			continue
		}
		if opp == nil {
			continue // price doesn't qualify right now
		}

		// Price qualifies — execute.
		log.Printf("[poll] ✓ %s | %s @ %.1f¢ | $%.2f",
			opp.Sport, opp.Side, opp.Price*100, opp.SizeUSDC)

		if err := exec.PlaceOrder(ctx, *opp); err != nil {
			log.Printf("[poll] order failed for %s: %v", opp.ConditionID[:12], err)
			continue
		}
		n.TradePlaced(opp.Market, opp.Side, opp.Sport, opp.Price, opp.SizeUSDC)
	}
}

// ── Safety nets ───────────────────────────────────────────────────────────────

func checkSafetyNets(cfg *config.Config, database *db.DB, bankroll float64, n *notify.Notifier) bool {
	// 1. Bankroll floor — hard stop, requires manual restart.
	if bankroll > 0 && bankroll < cfg.BankrollFloor {
		n.BankrollFloor(bankroll, cfg.BankrollFloor)
		log.Fatalf(
			"[safety] 🚨 BANKROLL FLOOR BREACHED — balance $%.2f < floor $%.2f — SHUTTING DOWN.",
			bankroll, cfg.BankrollFloor,
		)
	}

	// 2. Circuit breaker — check if one is already active.
	expiry, err := database.GetSetting("circuit_breaker_until")
	if err != nil {
		log.Printf("[safety] circuit breaker read error: %v", err)
	} else if expiry != "" {
		t, err := time.Parse(time.RFC3339, expiry)
		if err == nil && time.Now().UTC().Before(t) {
			log.Printf("[safety] ⏸ CIRCUIT BREAKER ACTIVE until %s UTC", t.Format("2006-01-02 15:04"))
			return true
		}
		if err == nil && time.Now().UTC().After(t) {
			_ = database.SetSetting("circuit_breaker_until", "")
			log.Printf("[safety] circuit breaker expired — trading resumed")
			n.CircuitBreakerCleared()
		}
	}

	// Trip a new circuit breaker if needed.
	consec, err := database.GetConsecutiveLosses()
	if err != nil {
		log.Printf("[safety] consecutive loss check error: %v", err)
	} else if consec >= cfg.ConsecLossLimit {
		until := time.Now().UTC().Add(24 * time.Hour)
		if err := database.SetSetting("circuit_breaker_until", until.Format(time.RFC3339)); err != nil {
			log.Printf("[safety] failed to set circuit breaker: %v", err)
		} else {
			log.Printf("[safety] 🚨 CIRCUIT BREAKER TRIPPED — %d consecutive losses — paused until %s UTC",
				consec, until.Format("2006-01-02 15:04"))
			n.CircuitBreaker(consec, until)
		}
		return true
	}

	// 3. Daily loss limit.
	dailyPnL, err := database.GetTodayPnL()
	if err != nil {
		log.Printf("[safety] daily P&L check error: %v", err)
	} else if dailyPnL < -cfg.MaxDailyLoss {
		log.Printf("[safety] 🚨 DAILY LOSS LIMIT HIT — today P&L: $%.2f (limit: -$%.2f)",
			dailyPnL, cfg.MaxDailyLoss)
		n.DailyLossLimit(dailyPnL, cfg.MaxDailyLoss)
		return true
	}

	return false
}

// ── Resolve open trades ───────────────────────────────────────────────────────

func resolveOpenTrades(ctx context.Context, database *db.DB, scanner *market.Scanner, n *notify.Notifier) {
	trades, err := database.GetOpenPaperTrades()
	if err != nil {
		log.Printf("[resolve] error fetching open trades: %v", err)
		return
	}
	if len(trades) == 0 {
		return
	}

	log.Printf("[resolve] checking %d open paper trade(s)", len(trades))
	resolved := 0

	for _, t := range trades {
		select {
		case <-ctx.Done():
			return
		default:
		}

		m, err := scanner.CheckMarket(t.ConditionID)
		if err != nil {
			log.Printf("[resolve] skip %s: %v", t.ConditionID[:12], err)
			continue
		}
		if m.Active && !m.Closed {
			continue
		}

		found, won := false, false
		for _, tok := range m.Tokens {
			if tok.Outcome != t.Side {
				continue
			}
			found = true
			won = tok.Winner || tok.Price >= 0.999
			break
		}
		if !found {
			continue
		}

		outcome := "LOSS"
		exitPrice := 0.0
		if won {
			outcome = "WIN"
			exitPrice = 1.0
		}

		pnl := t.Shares*exitPrice - t.SizeUSDC
		pnlPct := 0.0
		if t.SizeUSDC > 0 {
			pnlPct = pnl / t.SizeUSDC
		}

		if err := database.ResolvePaperTrade(t.ConditionID, outcome, exitPrice, pnl, pnlPct); err != nil {
			log.Printf("[resolve] DB update failed for %s: %v", t.ConditionID[:12], err)
			continue
		}

		log.Printf("[resolve] %s | %-30s | %s | P&L: $%.2f (%.1f%%)",
			outcome, t.Side, t.ConditionID[:12], pnl, pnlPct*100)
		n.TradeResolved(m.Question, t.Side, outcome, pnl)
		resolved++
	}

	if resolved > 0 {
		log.Printf("[resolve] %d trade(s) settled", resolved)
	}
}
