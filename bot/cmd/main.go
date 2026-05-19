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
	log.Printf("  Threshold: %.0f¢  Cap: $%.0f  Interval: %dm",
		cfg.EntryThreshold*100, cfg.MaxPositionSize, cfg.ScanIntervalMin)
	log.Printf("  DB:        %s", cfg.DBPath)
	log.Printf("══════════════════════════════════════════════")

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
	scanner := market.NewScanner(
		cfg.EntryThreshold,
		cfg.Sports,
		cfg.MaxPositionSize,
		cfg.MinHoursToClose,
	)

	// ── Run loop ──────────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Run immediately on start, then on the configured interval.
	tick := time.NewTicker(time.Duration(cfg.ScanIntervalMin) * time.Minute)
	defer tick.Stop()

	runScan(ctx, cfg, database, scanner, exec)

	for {
		select {
		case <-tick.C:
			runScan(ctx, cfg, database, scanner, exec)
		case <-ctx.Done():
			log.Println("shutting down")
			return
		}
	}
}

// runScan is one full scan-size-execute cycle.
func runScan(
	ctx context.Context,
	cfg *config.Config,
	database *db.DB,
	scanner *market.Scanner,
	exec executor.Executor,
) {
	log.Println("── scan start ──")

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
		}
	}

	log.Println("── scan end ──")
}
