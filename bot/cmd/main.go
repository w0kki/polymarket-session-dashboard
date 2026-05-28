package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/w0kki/polymarket-bot/internal/config"
	"github.com/w0kki/polymarket-bot/internal/db"
	"github.com/w0kki/polymarket-bot/internal/executor"
	"github.com/w0kki/polymarket-bot/internal/kelly"
	"github.com/w0kki/polymarket-bot/internal/market"
	"github.com/w0kki/polymarket-bot/internal/notify"
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
	log.Printf("  Threshold: %.0f¢  Cap: $%.0f  Interval: %ds",
		cfg.EntryThreshold*100, cfg.MaxPositionSize, cfg.ScanIntervalSec)
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
	// Build per-sport price bounds from config.
	// Sports not listed here fall back to the global EntryThreshold/MaxEntryPrice.
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
	log.Printf("  Tennis:    %.0f¢–%.0f¢  Baseball: %.0f¢–%.0f¢  Hockey: %.0f¢–%.0f¢",
		cfg.TennisMinPrice*100, cfg.TennisMaxPrice*100,
		cfg.BaseballMinPrice*100, cfg.BaseballMaxPrice*100,
		cfg.HockeyMinPrice*100, cfg.HockeyMaxPrice*100,
	)

	scanner := market.NewScanner(
		cfg.EntryThreshold,
		cfg.MaxEntryPrice,
		cfg.Sports,
		cfg.MaxPositionSize,
		cfg.MinHoursToClose,
		cfg.MaxHoursToClose,
		sportBounds,
	)

	// ── Run loop ──────────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run immediately on start, then on the configured interval.
	tick := time.NewTicker(time.Duration(cfg.ScanIntervalSec) * time.Second)
	defer tick.Stop()

	runScan(ctx, cfg, database, scanner, exec, notifier)

	for {
		select {
		case <-tick.C:
			runScan(ctx, cfg, database, scanner, exec, notifier)
		case <-ctx.Done():
			log.Println("shutting down")
			return
		}
	}
}

// checkSafetyNets enforces the three trading halts.
// Returns true if trading should be skipped this scan.
//
//  1. Bankroll floor   — shuts the process down entirely (log.Fatalf).
//  2. Circuit breaker  — 24-hour pause after N consecutive losses.
//  3. Daily loss limit — halts for the rest of the calendar day.
func checkSafetyNets(cfg *config.Config, database *db.DB, bankroll float64, n *notify.Notifier) bool {
	// 1. Bankroll floor — hard stop, requires manual restart
	if bankroll > 0 && bankroll < cfg.BankrollFloor {
		n.BankrollFloor(bankroll, cfg.BankrollFloor)
		log.Fatalf(
			"[safety] 🚨 BANKROLL FLOOR BREACHED — balance $%.2f < floor $%.2f — SHUTTING DOWN. Investigate before restarting.",
			bankroll, cfg.BankrollFloor,
		)
	}

	// 2. Consecutive loss circuit breaker — check if one is already active
	expiry, err := database.GetSetting("circuit_breaker_until")
	if err != nil {
		log.Printf("[safety] circuit breaker read error: %v", err)
	} else if expiry != "" {
		t, err := time.Parse(time.RFC3339, expiry)
		if err == nil && time.Now().UTC().Before(t) {
			log.Printf("[safety] ⏸  CIRCUIT BREAKER ACTIVE — trading paused until %s UTC",
				t.Format("2006-01-02 15:04"))
			return true
		}
		// Breaker expired — clear it so the log stays clean
		if err == nil && time.Now().UTC().After(t) {
			_ = database.SetSetting("circuit_breaker_until", "")
			log.Printf("[safety] circuit breaker expired — trading resumed")
			n.CircuitBreakerCleared()
		}
	}

	// Check whether we should trip a new circuit breaker
	consec, err := database.GetConsecutiveLosses()
	if err != nil {
		log.Printf("[safety] consecutive loss check error: %v", err)
	} else if consec >= cfg.ConsecLossLimit {
		until := time.Now().UTC().Add(24 * time.Hour)
		if err := database.SetSetting("circuit_breaker_until", until.Format(time.RFC3339)); err != nil {
			log.Printf("[safety] failed to set circuit breaker: %v", err)
		} else {
			log.Printf("[safety] 🚨 CIRCUIT BREAKER TRIPPED — %d consecutive losses — trading paused until %s UTC",
				consec, until.Format("2006-01-02 15:04"))
			n.CircuitBreaker(consec, until)
		}
		return true
	}

	// 3. Daily loss limit
	dailyPnL, err := database.GetTodayPnL()
	if err != nil {
		log.Printf("[safety] daily P&L check error: %v", err)
	} else if dailyPnL < -cfg.MaxDailyLoss {
		log.Printf("[safety] 🚨 DAILY LOSS LIMIT HIT — today P&L: $%.2f (limit: -$%.2f) — no more trades today",
			dailyPnL, cfg.MaxDailyLoss)
		n.DailyLossLimit(dailyPnL, cfg.MaxDailyLoss)
		return true
	}

	return false
}

// resolveOpenTrades checks every open paper trade against the CLOB API and
// writes WIN/LOSS + P&L to the DB when the market has settled.
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

		// Market must be closed/inactive before we resolve it
		if m.Active && !m.Closed {
			continue
		}

		// Find the token the bot bet on and determine outcome
		found := false
		won := false
		for _, tok := range m.Tokens {
			if tok.Outcome != t.Side {
				continue
			}
			found = true
			// Winner flag is set on resolution; price reaching 1.0 is a safe fallback
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

// runScan is one full scan-size-execute cycle.
func runScan(
	ctx context.Context,
	cfg *config.Config,
	database *db.DB,
	scanner *market.Scanner,
	exec executor.Executor,
	n *notify.Notifier,
) {
	log.Println("── scan start ──")

	// 0. Resolve any open paper trades first
	resolveOpenTrades(ctx, database, scanner, n)

	// 1. Load current bankroll from settings
	bankroll, err := database.GetBankroll()
	if err != nil {
		log.Printf("[scan] bankroll read error: %v — using fallback", err)
		bankroll = 0
	}
	if bankroll <= 0 {
		log.Printf("[scan] bankroll not set in DB, using fallback $%.0f", cfg.FallbackSize)
		bankroll = cfg.FallbackSize * 3 // rough estimate if not configured
	}

	// 2. Compute Kelly sizing from trade history
	stats, err := database.GetTradeStats()
	if err != nil {
		log.Printf("[scan] trade stats error: %v", err)
		stats = &db.TradeStats{}
	}

	kellyResult := kelly.Compute(
		stats.Wins, stats.Losses,
		stats.AvgWin, stats.AvgLoss,
		bankroll, cfg.MaxPositionSize, cfg.FallbackSize,
	)

	if kellyResult.Computed {
		log.Printf("[kelly] full=%.1f%%  half=%.1f%%  size=$%.2f  (bankroll=$%.0f)",
			kellyResult.FullKelly*100, kellyResult.HalfKelly*100,
			kellyResult.PositionSize, bankroll)
	} else {
		log.Printf("[kelly] insufficient loss data — fallback size=$%.2f", kellyResult.PositionSize)
	}

	// Safety nets — halt trading if any circuit breaker is active.
	// Open trade resolution above always runs; only new entries are blocked.
	if checkSafetyNets(cfg, database, bankroll, n) {
		log.Println("── scan end (halted by safety net) ──")
		return
	}

	// Sizer function: Kelly result is constant per scan regardless of price.
	// (Price only affects shares count, not dollar size.)
	sizer := func(_ float64) float64 { return kellyResult.PositionSize }

	// 3. Build deduplication sets
	traded, err := database.ActiveConditionIDs()
	if err != nil {
		log.Printf("[scan] active positions error: %v", err)
		traded = map[string]bool{}
	}
	// Also check trades table to avoid re-entering paper trades
	// (ActiveConditionIDs only covers the positions table)

	// 4. Find qualifying opportunities
	opps, err := scanner.Scan(traded, traded, sizer)
	if err != nil {
		log.Printf("[scan] scanner error: %v", err)
		return
	}

	if len(opps) == 0 {
		log.Println("[scan] no qualifying opportunities")
		log.Println("── scan end ──")
		return
	}

	log.Printf("[scan] %d opportunit(ies) found", len(opps))

	// 5. Execute each opportunity
	for _, opp := range opps {
		// Final dedup check against trades table (catches paper trades too)
		already, err := database.IsAlreadyTraded(opp.ConditionID)
		if err != nil {
			log.Printf("[scan] dedup check error for %s: %v", opp.ConditionID[:12], err)
			continue
		}
		if already {
			log.Printf("[scan] skip %s — already in trades table", opp.ConditionID[:12])
			continue
		}

		if err := exec.PlaceOrder(ctx, opp); err != nil {
			log.Printf("[scan] order failed for %s: %v", opp.ConditionID[:12], err)
			continue
		}
		n.TradePlaced(opp.Market, opp.Side, opp.Sport, opp.Price, opp.SizeUSDC)
	}

	log.Println("── scan end ──")
}
