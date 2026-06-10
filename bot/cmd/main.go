package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// orderFailCooldown tracks the last time an order failed for a given
// conditionID. If the same market fails again within orderFailCooldownDur,
// the bot skips it silently to prevent alert spam every 10 seconds.
var (
	orderFailMu       sync.Mutex
	orderFailAt       = map[string]time.Time{}
	orderFailCooldown = 5 * time.Minute
)

// placedThisSession tracks every conditionID the bot has submitted an order for
// during this process lifetime. It guarantees at-most-one order attempt per
// market per session — the in-memory guard that prevents the multi-fill bug
// (the bot re-firing every poll while a "delayed" order settles). Cross-session
// dedup is handled by IsAlreadyTraded against the synced DB.
var (
	placedMu sync.Mutex
	placed   = map[string]bool{}
)

// floorBreachStreak counts consecutive safety checks that saw balance < floor.
// The bankroll floor only HALTS after floorBreachConfirm consecutive readings,
// so a single transient data-feed glitch (e.g. data-api /value momentarily
// returning 0 open positions, which understates the balance to cash-only) can't
// false-halt the bot. A real breach persists and confirms within a few cycles.
var (
	floorBreachStreak  int
	floorBreachConfirm = 3
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
	log.Printf("  Discovery: every %ds  Poll: every %ds  Stop-loss: every %ds", cfg.ScanIntervalSec, cfg.PollIntervalSec, cfg.StopLossIntervalSec)
	log.Printf("  DB:        %s", cfg.DBPath)
	log.Printf("══════════════════════════════════════════════")

	// ── Notifier ─────────────────────────────────────────────────────────────
	notifier := notify.New(cfg.DiscordWebhookURL, cfg.TelegramBotToken, cfg.TelegramChatID, cfg.DryRun)
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

	// ── Mode override (set via Telegram /live or /paper command) ──────────────
	// Allows switching modes without editing ecosystem.config.cjs manually.
	if override, _ := database.GetSetting("mode_override"); override != "" {
		switch override {
		case "live":
			cfg.DryRun = false
			log.Printf("  [mode_override] LIVE mode active (set via Telegram /live)")
		case "paper":
			cfg.DryRun = true
			log.Printf("  [mode_override] PAPER mode active (set via Telegram /paper)")
		}
		// Re-log the effective mode and sync the notifier label.
		mode = "PAPER"
		if !cfg.DryRun {
			mode = "LIVE"
		}
		notifier.SetMode(cfg.DryRun)
		log.Printf("  Effective mode: %s", mode)
	}

	// ── Signal handling ───────────────────────────────────────────────────────
	// Set up early so ctx is available for both the halted path and the normal
	// trading path below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Kill-switch check ─────────────────────────────────────────────────────
	// If /kill was issued, the bot restarts itself via SIGTERM and re-enters
	// this block in "halted" mode: only the Telegram listener runs, no trading.
	// /startup clears the flag and triggers another restart → normal operation.
	// pm2 always has a live process (crash recovery still works) but all
	// trading is frozen until explicitly resumed.
	if killed, _ := database.GetSetting("bot_killed"); killed == "true" {
		log.Printf("  [HALTED] Kill switch active — Telegram listener only, no trading")
		if notifier.Enabled() {
			notifier.Broadcast("🔴 Bot is HALTED — trading suspended.\nSend /startup to resume.")
		}
		notifier.ListenCommands(ctx, makeCmdHandler(cfg, database, notifier))
		<-ctx.Done()
		log.Println("shutting down (halted mode)")
		return
	}

	// ── Startup notification ──────────────────────────────────────────────────
	// Send a brief ping so the operator knows the bot came back up after a
	// restart. Fires after mode_override and kill-switch checks so the label
	// is accurate and only fires when actually entering trading mode.
	if notifier.Enabled() {
		modeLabel := "📋 PAPER"
		if !cfg.DryRun {
			modeLabel = "🟢 LIVE"
		}
		notifier.Broadcast(fmt.Sprintf("🚀 Bot started — %s mode", modeLabel))
	}

	// ── Executor ──────────────────────────────────────────────────────────────
	var exec executor.Executor
	if cfg.DryRun {
		exec = executor.NewPaper(database)
	} else {
		live, err := executor.NewLive(
			cfg.PolyPrivateKey,
			cfg.PolyAPIKey,
			cfg.PolyAPISecret,
			cfg.PolyAPIPassphrase,
			cfg.PolyProxyWallet,
			database,
		)
		if err != nil {
			log.Fatalf("live executor init failed: %v", err)
		}
		// Verify credentials against the CLOB before accepting any orders.
		if err := live.VerifyCredentials(context.Background()); err != nil {
			log.Fatalf("CLOB credential check failed: %v", err)
		}
		exec = live
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
		"Soccer": {
			MinPrice:        cfg.SoccerMinPrice,
			MaxPrice:        cfg.SoccerMaxPrice,
			MaxHoursToClose: cfg.SoccerMaxHoursToClose,
		},
	}
	if cfg.StopLossDrop > 0 {
		log.Printf("  Stop Loss: enabled — exit if price drops %.0f¢ from entry", cfg.StopLossDrop*100)
	} else {
		log.Printf("  Stop Loss: disabled")
	}
	for _, s := range []string{"Tennis", "Baseball", "Hockey", "Basketball", "Soccer"} {
		if d, ok := cfg.StopLossDropBySport[s]; ok {
			if d <= 0 {
				log.Printf("  Stop Loss [%s]: DISABLED (hold to settlement)", s)
			} else {
				log.Printf("  Stop Loss [%s]: %.0f¢ drop", s, d*100)
			}
		}
	}
	log.Printf("  Tennis: %.0f¢–%.0f¢  Baseball: %.0f¢–%.0f¢  Hockey: %.0f¢–%.0f¢",
		cfg.TennisMinPrice*100, cfg.TennisMaxPrice*100,
		cfg.BaseballMinPrice*100, cfg.BaseballMaxPrice*100,
		cfg.HockeyMinPrice*100, cfg.HockeyMaxPrice*100,
	)
	if cfg.SoccerMinHalf > 0 || cfg.SoccerGoalDiff > 0 {
		log.Printf("  Soccer: %.0f¢–%.0f¢  ((half≥%d AND diff≥%d) OR diff≥%d)",
			cfg.SoccerMinPrice*100, cfg.SoccerMaxPrice*100,
			cfg.SoccerMinHalf, cfg.SoccerMinGoalDiff, cfg.SoccerGoalDiff,
		)
	} else {
		log.Printf("  Soccer: %.0f¢–%.0f¢  (no game-state gate)",
			cfg.SoccerMinPrice*100, cfg.SoccerMaxPrice*100)
	}
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
		cfg.PaperTradeDoubles,
	)
	if cfg.PaperTradeDoubles {
		log.Printf("  Doubles: PAPER-ONLY (tennis doubles routed to paper executor for evaluation)")
	}

	// A paper executor is always available so paper-only markets (e.g. doubles)
	// can be routed to it even while the bot trades live.
	paperExec := executor.NewPaper(database)

	// ── Telegram command listener ─────────────────────────────────────────────
	notifier.ListenCommands(ctx, makeCmdHandler(cfg, database, notifier))

	// ── Two-speed loop ────────────────────────────────────────────────────────
	// 1. Discovery (slow): full market scan → rebuilds watchlist + resolves trades
	// 2. Poll (fast): checks each watchlisted market for a qualifying price
	//
	// Discovery runs first so the watchlist is populated before polling starts.
	log.Printf("[discovery] initial scan starting...")
	runDiscovery(ctx, cfg, database, scanner, notifier)

	discoveryTicker := time.NewTicker(time.Duration(cfg.ScanIntervalSec) * time.Second)
	pollTicker := time.NewTicker(time.Duration(cfg.PollIntervalSec) * time.Second)
	stopTicker := time.NewTicker(time.Duration(cfg.StopLossIntervalSec) * time.Second)
	defer discoveryTicker.Stop()
	defer pollTicker.Stop()
	defer stopTicker.Stop()

	// "running" guards prevent a slow pass from piling up if its ticker fires
	// again before it finishes. The entry scan staggers across all markets and
	// can take 30s+; running it (and the stop-loss) off the select loop keeps the
	// stop-loss ticker firing on time so exits aren't blocked behind the scan.
	var pollRunning, stopRunning atomic.Bool

	for {
		select {
		case <-discoveryTicker.C:
			go runDiscovery(ctx, cfg, database, scanner, notifier)
		case <-pollTicker.C:
			if pollRunning.CompareAndSwap(false, true) {
				go func() {
					defer pollRunning.Store(false)
					runPoll(ctx, cfg, database, scanner, exec, paperExec, notifier)
				}()
			}
		case <-stopTicker.C:
			// Dedicated fast stop-loss check, decoupled from the entry scan so a
			// collapsing position is exited within StopLossIntervalSec rather than
			// a full ~30s poll cycle.
			if stopRunning.CompareAndSwap(false, true) {
				go func() {
					defer stopRunning.Store(false)
					runStopLoss(ctx, cfg, database, scanner, exec, notifier)
				}()
			}
		case <-ctx.Done():
			log.Println("shutting down")
			return
		}
	}
}

// helpText returns the canonical command list shown by /help and the unknown-
// command fallback. Keep this synced with the command switch in the Telegram
// listener — if you add a command, add it here too.
func helpText() string {
	return "📖 BOT COMMANDS\n\n" +
		"State & info\n" +
		"/status — bot state, balance, P&L, current streak\n" +
		"/help — show this message\n\n" +
		"Trading control\n" +
		"/live — switch to LIVE trading (real money)\n" +
		"/paper — switch to PAPER trading (no real orders)\n" +
		"/kill — halt all trading (bot stays alive, dormant)\n" +
		"/startup — resume trading after /kill; also clears today's daily-loss halt + circuit breaker\n" +
		"/stop — graceful shutdown (pm2 restarts automatically)\n\n" +
		"Tuning (live, no restart needed)\n" +
		"/bankroll <amount> — update bankroll baseline, e.g. /bankroll 2500\n" +
		"/fallback <amount> — update per-trade size, e.g. /fallback 100\n" +
		"/stoploss <cents> — update global stop-loss drop, e.g. /stoploss 40\n" +
		"/clearbreaker — clear the circuit-breaker halt"
}

// evictFromWatchlist removes a single market from the in-memory watchlist.
// Called when order placement returns order_version_mismatch — the CLOB has
// closed the market but hasn't updated accepting_orders yet. The market will be
// splitMatchTeams parses a Polymarket market question like
// "Boston Red Sox vs. New York Yankees" and returns (homeTeam, awayTeam).
//
// IMPORTANT: Polymarket lists the AWAY team FIRST (matches MLB slug convention
// like "mlb-bos-nyy-2026-06-06" where Boston is at Yankees). So "X vs. Y" means
// X is away, Y is home. The function returns them in (home, away) order to
// match the official feed schema.
//
// Returns ("", "") if the question can't be parsed.
func splitMatchTeams(question string) (home, away string) {
	q := question
	// Polymarket format: "Away vs. Home" — try " vs. " first, fall back to " vs ".
	for _, sep := range []string{" vs. ", " vs "} {
		if idx := strings.Index(strings.ToLower(q), sep); idx >= 0 {
			away = strings.TrimSpace(q[:idx])
			home = strings.TrimSpace(q[idx+len(sep):])
			// Some markets append " — Moneyline" or similar; strip after a long-dash or colon.
			for _, suffix := range []string{" —", " -", ":"} {
				if i := strings.Index(home, suffix); i >= 0 {
					home = strings.TrimSpace(home[:i])
				}
			}
			return home, away
		}
	}
	return "", ""
}

// naturally excluded on the next BuildWatchlist run (~10 min).
func evictFromWatchlist(conditionID string) {
	watchlistMu.Lock()
	defer watchlistMu.Unlock()
	for i, e := range watchlist {
		if e.ConditionID == conditionID {
			watchlist = append(watchlist[:i], watchlist[i+1:]...)
			return
		}
	}
}

// ── Discovery loop ────────────────────────────────────────────────────────────

// runDiscovery does a full market scan, resolves open trades, and rebuilds
// the watchlist. Runs every ~10 minutes (SCAN_INTERVAL_SEC).
func runDiscovery(ctx context.Context, cfg *config.Config, database *db.DB, scanner *market.Scanner, n *notify.Notifier) {
	log.Println("── discovery start ──")

	// Resolve any open paper trades first, then live trades.
	resolveOpenTrades(ctx, database, scanner, n)
	resolveLiveTrades(ctx, database, scanner, n)

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
func runPoll(ctx context.Context, cfg *config.Config, database *db.DB, scanner *market.Scanner, exec, paperExec executor.Executor, n *notify.Notifier) {
	// Snapshot the watchlist under read lock.
	watchlistMu.RLock()
	entries := make([]market.WatchlistEntry, len(watchlist))
	copy(entries, watchlist)
	watchlistMu.RUnlock()

	if len(entries) == 0 {
		return // discovery hasn't run yet
	}

	// Stop-loss now runs on its own dedicated ticker (see the main loop), so it
	// is no longer gated behind this entry scan.

	// Load bankroll and compute current balance.
	// balance = bankroll + live P&L earned AFTER the bankroll was last set.
	// Trades resolved before the bankroll was updated are already reflected in
	// the bankroll figure itself, so we exclude them to avoid double-counting.
	fallback := effectiveFallback(cfg, database)
	bankroll, bankrollSince, err := database.GetBankroll()
	if err != nil || bankroll <= 0 {
		bankroll = fallback * 3
		bankrollSince = ""
	}

	// currentBalance is what the bankroll-floor safety net checks against.
	// Prefer the REAL on-chain account value (cash + open positions) when live —
	// it reflects actual money and is immune to ledger timing quirks (e.g. a
	// backlog of old positions settling at once and double-counting against a
	// fresh baseline, which previously caused a false floor breach). Fall back to
	// the derived ledger figure only for paper mode. If the on-chain lookup fails
	// transiently we mark the balance UNKNOWN so the floor is skipped this cycle —
	// never halt on a fetch error.
	livePnLSince, _ := database.GetLivePnLSince(bankrollSince)
	currentBalance := bankroll + livePnLSince
	balanceKnown := true
	if br, ok := exec.(executor.BalanceReporter); ok {
		if cash, positions, berr := br.Balance(ctx); berr == nil {
			currentBalance = cash + positions
		} else {
			log.Printf("[safety] on-chain balance lookup failed — skipping bankroll-floor check this cycle: %v", berr)
			balanceKnown = false
		}
	}
	if balanceKnown {
		// Cache for /status (and so it's available in dormant mode).
		_ = database.SetSetting("last_balance", fmt.Sprintf("%.2f", currentBalance))
	}

	stats, err := database.GetTradeStats()
	if err != nil {
		stats = &db.TradeStats{}
	}
	kellyResult := kelly.Compute(
		stats.Wins, stats.Losses,
		stats.AvgWin, stats.AvgLoss,
		bankroll, cfg.MaxPositionSize, fallback,
	)
	sizer := func(_ float64) float64 { return kellyResult.PositionSize }

	// Check safety nets before entering any new positions.
	if checkSafetyNets(cfg, database, bankroll, currentBalance, balanceKnown, n) {
		return
	}

	// Stagger requests to avoid CLOB rate-limits (HTTP 429).
	// Spread all markets evenly across the poll interval so the CLOB sees a
	// steady trickle rather than a burst of 100+ requests fired simultaneously.
	// e.g. 144 markets, 10s interval → ~69 ms between requests ≈ 14 req/s.
	// Floor at 50 ms so a tiny watchlist doesn't spin too hot.
	stagger := time.Duration(cfg.PollIntervalSec) * time.Second / time.Duration(len(entries))
	if stagger < 50*time.Millisecond {
		stagger = 50 * time.Millisecond
	}

	// Poll each watchlisted market.
	for i, entry := range entries {
		// Sleep before every market except the first — spreads the burst.
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(stagger):
			}
		}

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

		// Session-level dedup: never attempt the same market twice in one process
		// lifetime (guards against re-firing while a "delayed" order settles).
		placedMu.Lock()
		alreadyPlaced := placed[entry.ConditionID]
		placedMu.Unlock()
		if alreadyPlaced {
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

		// Tennis set-stage gate: only enter late in the match (end of 2nd set
		// or during the 3rd+), using live state from the sports_collector sidecar.
		if opp.Sport == "Tennis" && cfg.TennisMinSet > 0 {
			gameID, ok := scanner.ResolveGameID(opp.Slug)
			if !ok {
				log.Printf("[poll] tennis gate: no gameId for %s — skipping", opp.Slug)
				continue
			}
			ls, _ := database.GetLiveSport(gameID)
			if ls == nil || !ls.Live {
				log.Printf("[poll] tennis gate: no live state for game %d (%s) — skipping", gameID, opp.Side)
				continue
			}
			if !market.TennisSetStageOK(ls.Period, ls.Score, cfg.TennisMinSet) {
				log.Printf("[poll] tennis gate: game %d in %s (score %q) — below set %d, skipping %s",
					gameID, ls.Period, ls.Score, cfg.TennisMinSet, opp.Side)
				continue
			}
			log.Printf("[poll] tennis gate: ✓ game %d in %s (score %q) — set ≥ %d OK",
				gameID, ls.Period, ls.Score, cfg.TennisMinSet)
		}

		// Baseball game-stage gate (Option B):
		//   PASS if inning >= BaseballMinInning AND diff >= BaseballMinRunDiff
		//   PASS if diff >= BaseballRunDiff  (blowout bypass, any inning)
		//
		// Two-feed strategy: read both Polymarket's sports feed AND the official
		// MLB Stats feed (via official_collector). Polymarket's feed lags real
		// time by 30s–2min and has been observed showing the wrong state during
		// fast innings. We REQUIRE BOTH feeds to pass the gate; if the official
		// feed is unavailable, fall back to Polymarket alone (graceful degrade).
		if opp.Sport == "Baseball" && (cfg.BaseballMinInning > 0 || cfg.BaseballRunDiff > 0) {
			gameID, ok := scanner.ResolveGameID(opp.Slug)
			if !ok {
				log.Printf("[poll] baseball gate: no gameId for %s — skipping", opp.Slug)
				continue
			}
			ls, _ := database.GetLiveSport(gameID)
			if ls == nil || !ls.Live {
				log.Printf("[poll] baseball gate: no live state for game %d (%s) — skipping", gameID, opp.Side)
				continue
			}
			if !market.GameStageOK(ls.Period, ls.Score, cfg.BaseballMinInning, cfg.BaseballRunDiff, cfg.BaseballMinRunDiff) {
				log.Printf("[poll] baseball gate: game %d in %q (score %q) — below inning %d+diff≥%d / blowout≥%d, skipping %s",
					gameID, ls.Period, ls.Score, cfg.BaseballMinInning, cfg.BaseballMinRunDiff, cfg.BaseballRunDiff, opp.Side)
				continue
			}
			// Second check: official MLB feed must also confirm.
			homeTeam, awayTeam := splitMatchTeams(opp.Market)
			lso, _ := database.GetLiveSportOfficial("Baseball", homeTeam, awayTeam)
			if lso != nil {
				if lso.Ended {
					log.Printf("[poll] baseball gate: ✗ official feed says ENDED for %s vs %s — skipping %s",
						homeTeam, awayTeam, opp.Side)
					continue
				}
				if !market.GameStageOK(lso.Period, lso.Score, cfg.BaseballMinInning, cfg.BaseballRunDiff, cfg.BaseballMinRunDiff) {
					log.Printf("[poll] baseball gate: ✗ official feed disagrees — Poly says %q/%q OK, MLB says %q/%q — skipping %s",
						ls.Period, ls.Score, lso.Period, lso.Score, opp.Side)
					continue
				}
				log.Printf("[poll] baseball gate: ✓ game %d in %q (score %q) — Poly+MLB agree (MLB: %q/%q) OK",
					gameID, ls.Period, ls.Score, lso.Period, lso.Score)
			} else {
				log.Printf("[poll] baseball gate: ✓ game %d in %q (score %q) — Poly only (no official feed match for %s vs %s) OK",
					gameID, ls.Period, ls.Score, homeTeam, awayTeam)
			}
		}

		// Hockey game-stage gate: only enter from the min period on, or once the
		// goal differential reaches the blowout threshold.
		if opp.Sport == "Hockey" && (cfg.HockeyMinPeriod > 0 || cfg.HockeyGoalDiff > 0) {
			gameID, ok := scanner.ResolveGameID(opp.Slug)
			if !ok {
				log.Printf("[poll] hockey gate: no gameId for %s — skipping", opp.Slug)
				continue
			}
			ls, _ := database.GetLiveSport(gameID)
			if ls == nil || !ls.Live {
				log.Printf("[poll] hockey gate: no live state for game %d (%s) — skipping", gameID, opp.Side)
				continue
			}
			if !market.GameStageOK(ls.Period, ls.Score, cfg.HockeyMinPeriod, cfg.HockeyGoalDiff, 0) {
				log.Printf("[poll] hockey gate: game %d in %q (score %q) — below period %d / goal-diff %d, skipping %s",
					gameID, ls.Period, ls.Score, cfg.HockeyMinPeriod, cfg.HockeyGoalDiff, opp.Side)
				continue
			}
			log.Printf("[poll] hockey gate: ✓ game %d in %q (score %q) — period≥%d or goalDiff≥%d OK",
				gameID, ls.Period, ls.Score, cfg.HockeyMinPeriod, cfg.HockeyGoalDiff)
		}

		// Basketball game-stage gate (NBA + WNBA): only enter from the min
		// quarter on, or once the point differential reaches the blowout margin.
		if opp.Sport == "Basketball" && (cfg.BasketballMinQuarter > 0 || cfg.BasketballPointDiff > 0) {
			gameID, ok := scanner.ResolveGameID(opp.Slug)
			if !ok {
				log.Printf("[poll] basketball gate: no gameId for %s — skipping", opp.Slug)
				continue
			}
			ls, _ := database.GetLiveSport(gameID)
			if ls == nil || !ls.Live {
				log.Printf("[poll] basketball gate: no live state for game %d (%s) — skipping", gameID, opp.Side)
				continue
			}
			if !market.GameStageOK(ls.Period, ls.Score, cfg.BasketballMinQuarter, cfg.BasketballPointDiff, cfg.BasketballMinPointDiff) {
				log.Printf("[poll] basketball gate: game %d in %q (score %q) — below quarter %d+diff≥%d / blowout≥%d, skipping %s",
					gameID, ls.Period, ls.Score, cfg.BasketballMinQuarter, cfg.BasketballMinPointDiff, cfg.BasketballPointDiff, opp.Side)
				continue
			}
			log.Printf("[poll] basketball gate: ✓ game %d in %q (score %q) — (quarter≥%d AND diff≥%d) or blowout≥%d OK",
				gameID, ls.Period, ls.Score, cfg.BasketballMinQuarter, cfg.BasketballMinPointDiff, cfg.BasketballPointDiff)
		}

		// Soccer game-stage gate (Path A — half-of-match + goal-diff):
		//   PASS if half >= SoccerMinHalf AND diff >= SoccerMinGoalDiff
		//   PASS if diff >= SoccerGoalDiff  (blowout bypass, any half)
		// Uses live_sports.period which reports "1H"/"2H"/"VFT" for soccer.
		// Replaces the broken end_date_iso time-window check (Polymarket sets
		// end_date_iso to midnight of match date, not real match clock).
		if opp.Sport == "Soccer" && (cfg.SoccerMinHalf > 0 || cfg.SoccerGoalDiff > 0) {
			gameID, ok := scanner.ResolveGameID(opp.Slug)
			if !ok {
				log.Printf("[poll] soccer gate: no gameId for %s — skipping", opp.Slug)
				continue
			}
			ls, _ := database.GetLiveSport(gameID)
			if ls == nil || !ls.Live {
				log.Printf("[poll] soccer gate: no live state for game %d (%s) — skipping", gameID, opp.Side)
				continue
			}
			if !market.GameStageOK(ls.Period, ls.Score, cfg.SoccerMinHalf, cfg.SoccerGoalDiff, cfg.SoccerMinGoalDiff) {
				log.Printf("[poll] soccer gate: game %d in %q (score %q) — below half %d+diff≥%d / blowout≥%d, skipping %s",
					gameID, ls.Period, ls.Score, cfg.SoccerMinHalf, cfg.SoccerMinGoalDiff, cfg.SoccerGoalDiff, opp.Side)
				continue
			}
			log.Printf("[poll] soccer gate: ✓ game %d in %q (score %q) — (half≥%d AND diff≥%d) or blowout≥%d OK",
				gameID, ls.Period, ls.Score, cfg.SoccerMinHalf, cfg.SoccerMinGoalDiff, cfg.SoccerGoalDiff)
		}

		// Route paper-only opportunities (e.g. tennis doubles) to the paper
		// executor so they NEVER touch live capital, regardless of live/paper mode.
		placeExec := exec
		tag := ""
		if opp.PaperOnly {
			placeExec = paperExec
			tag = " [PAPER-ONLY]"
		}

		// Price qualifies — execute. Mark the market as placed BEFORE firing so a
		// concurrent/next poll can't double-submit while the fill is confirmed.
		log.Printf("[poll] ✓ %s | %s @ %.1f¢ | $%.2f%s",
			opp.Sport, opp.Side, opp.Price*100, opp.SizeUSDC, tag)
		placedMu.Lock()
		placed[opp.ConditionID] = true
		placedMu.Unlock()

		if err := placeExec.PlaceOrder(ctx, *opp); err != nil {
			// Order accepted by the CLOB but no on-chain fill confirmed — keep it
			// deduped (already marked placed), record nothing, don't alert.
			if errors.Is(err, executor.ErrOrderUnconfirmed) {
				log.Printf("[poll] order unconfirmed for %s (%s) — no fill on-chain, skipping", opp.ConditionID[:12], opp.Side)
				continue
			}
			log.Printf("[poll] order failed for %s: %v", opp.ConditionID[:12], err)

			errStr := err.Error()

			// Evict markets that produce unrecoverable errors so we don't spam
			// retries every 10 s until the next full discovery scan.
			//   order_version_mismatch — market closed between scan and now.
			//   order not filled       — FAK returned status:delayed meaning no
			//                           liquidity at this price right now; retrying
			//                           every 10 s won't help.
			if strings.Contains(errStr, "order_version_mismatch") ||
				strings.Contains(errStr, "order not filled") {
				evictFromWatchlist(opp.ConditionID)
				log.Printf("[poll] evicted %s from watchlist (%s)", opp.ConditionID[:12], errStr[:40])
				continue
			}

			// Cooldown: only alert once per market per 5 minutes to prevent
			// notification spam on repeated poll cycles.
			orderFailMu.Lock()
			lastFail, seen := orderFailAt[opp.ConditionID]
			if !seen || time.Since(lastFail) >= orderFailCooldown {
				orderFailAt[opp.ConditionID] = time.Now()
				orderFailMu.Unlock()

				if strings.Contains(errStr, "insufficient") || strings.Contains(errStr, "balance") || strings.Contains(errStr, "funds") {
					n.Broadcast(fmt.Sprintf(
						"⚠️ ORDER SKIPPED — insufficient USDC\n%s · %s @ %.1f¢ · $%.2f\nDeposit more USDC or reduce position size.",
						opp.Sport, opp.Side, opp.Price*100, opp.SizeUSDC,
					))
				} else {
					n.Broadcast(fmt.Sprintf(
						"⚠️ ORDER FAILED — %s · %s @ %.1f¢\nError: %s",
						opp.Sport, opp.Side, opp.Price*100, errStr,
					))
				}
			} else {
				orderFailMu.Unlock()
				log.Printf("[poll] order failure suppressed (cooldown) for %s", opp.ConditionID[:12])
			}
			continue
		}
		// Clear any failure cooldown on success.
		orderFailMu.Lock()
		delete(orderFailAt, opp.ConditionID)
		orderFailMu.Unlock()
		if opp.PaperOnly {
			// Don't fire the (live-labelled) trade alert for paper-only trades —
			// they're recorded as Paper in the DB and visible on the dashboard.
			log.Printf("[poll] 📝 PAPER-ONLY recorded: %s | %s @ %.1f¢ | $%.2f", opp.Sport, opp.Side, opp.Price*100, opp.SizeUSDC)
		} else {
			n.TradePlaced(opp.Market, opp.Side, opp.Sport, opp.Slug, opp.Price, opp.SizeUSDC)
		}
	}
}

// ── Safety nets ───────────────────────────────────────────────────────────────

func checkSafetyNets(cfg *config.Config, database *db.DB, bankroll, currentBalance float64, balanceKnown bool, n *notify.Notifier) bool {
	// 1. Bankroll floor — hard stop requiring manual intervention.
	// Floor = configured bankroll × BankrollFloorPct (default 50%).
	// currentBalance = bankroll + live-trade P&L only (paper excluded),
	// so it tracks real money movement against the wallet balance.
	//
	// IMPORTANT: enter DORMANT mode (set bot_killed) rather than exiting. Under
	// pm2 autorestart a plain os.Exit/Fatalf would immediately come back up,
	// re-breach, and crash-loop while spamming the alert. Setting bot_killed
	// first means the restarted process boots straight into halted mode (Telegram
	// listener only, no trading) — so it alerts exactly once. Resume with /startup.
	floor := bankroll * cfg.BankrollFloorPct
	if balanceKnown && bankroll > 0 && currentBalance < floor {
		// Confirm before halting: a single low reading is usually a transient feed
		// glitch (e.g. data-api momentarily reporting 0 open positions, leaving only
		// cash), not a real drawdown. Require floorBreachConfirm consecutive breaches.
		floorBreachStreak++
		if floorBreachStreak < floorBreachConfirm {
			log.Printf(
				"[safety] ⚠️ balance $%.2f < floor $%.2f (reading %d/%d) — pausing new entries, confirming before halt (guards against transient balance-feed glitches).",
				currentBalance, floor, floorBreachStreak, floorBreachConfirm,
			)
			return true // skip new entries this cycle, but do NOT go dormant yet
		}
		halted, _ := database.GetSetting("bot_killed")
		if halted != "true" {
			_ = database.SetSetting("bot_killed", "true")
			n.BankrollFloor(currentBalance, floor)
			log.Printf(
				"[safety] 🚨 BANKROLL FLOOR BREACHED (confirmed %d×) — balance $%.2f < floor $%.2f (%.0f%% of $%.2f) — entering DORMANT mode (resume with /startup).",
				floorBreachStreak, currentBalance, floor, cfg.BankrollFloorPct*100, bankroll,
			)
			// Restart into halted mode.
			if p, err := os.FindProcess(os.Getpid()); err == nil {
				_ = p.Signal(syscall.SIGTERM)
			}
		}
		return true
	}
	// Healthy (or unknown) reading — reset the breach streak.
	floorBreachStreak = 0

	// Same-day safety override (set by /startup) bypasses the soft halts — the
	// circuit breaker and daily loss limit — until midnight UTC. The bankroll
	// floor above is NOT overridden.
	today := time.Now().UTC().Format("2006-01-02")
	overrideDate, _ := database.GetSetting("safety_override")
	safetyOverride := overrideDate == today

	// 2. Circuit breaker — check if one is already active.
	expiry, err := database.GetSetting("circuit_breaker_until")
	if err != nil {
		log.Printf("[safety] circuit breaker read error: %v", err)
	} else if expiry != "" && !safetyOverride {
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

	// Trip a new circuit breaker if needed (unless overridden via /startup).
	consec, err := database.GetConsecutiveLosses()
	if err != nil {
		log.Printf("[safety] consecutive loss check error: %v", err)
	} else if consec >= cfg.ConsecLossLimit && !safetyOverride {
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

	// 3. Daily loss limit. Halt trading for the rest of the UTC day, but only
	// ALERT once — the check runs every poll, so without a guard it spams the
	// notification continuously. daily_loss_halt stores the date it was tripped.
	dailyPnL, err := database.GetTodayPnL()
	if err != nil {
		log.Printf("[safety] daily P&L check error: %v", err)
	} else if dailyPnL < -cfg.MaxDailyLoss && !safetyOverride {
		haltDate, _ := database.GetSetting("daily_loss_halt")
		if haltDate != today {
			_ = database.SetSetting("daily_loss_halt", today)
			log.Printf("[safety] 🚨 DAILY LOSS LIMIT HIT — today P&L: $%.2f (limit: -$%.2f) — halting until midnight UTC",
				dailyPnL, cfg.MaxDailyLoss)
			n.DailyLossLimit(dailyPnL, cfg.MaxDailyLoss)
		} else {
			log.Printf("[safety] ⏸ daily loss limit active (P&L $%.2f) — trading halted until midnight UTC", dailyPnL)
		}
		return true
	}

	return false
}

// ── Stop loss ─────────────────────────────────────────────────────────────────

// effectiveStopLoss returns the live stop-loss drop (price fraction, e.g.
// 0.40 = 40¢) for a given sport. Precedence:
//  1. Per-sport override (<SPORT>_STOP_LOSS_DROP env) — including 0, which
//     DISABLES the stop for that sport (hold to settlement, e.g. Tennis).
//  2. The global runtime override from the settings table (Telegram /stoploss).
//  3. The global STOP_LOSS_DROP env default.
//
// Pass sport="" for the global value (e.g. /status, /stoploss).
func effectiveStopLoss(cfg *config.Config, database *db.DB, sport string) float64 {
	if d, ok := cfg.StopLossDropBySport[sport]; ok {
		return d // per-sport wins, 0 = disabled for this sport
	}
	if raw, _ := database.GetSetting("stop_loss_drop"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			return v
		}
	}
	return cfg.StopLossDrop
}

// effectiveFallback returns the live fallback position size: the Telegram-set
// override (fallback_size setting) if present, otherwise the configured default.
// The fallback is used to size a trade when Kelly can't be computed (not enough
// loss data, or a non-positive edge).
func effectiveFallback(cfg *config.Config, database *db.DB) float64 {
	if raw, _ := database.GetSetting("fallback_size"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 {
			return v
		}
	}
	return cfg.FallbackSize
}

// runStopLoss checks every open trade (paper and live) against the configured
// stop loss price. For paper trades it updates the DB directly. For live trades
// it places a real SELL order on the CLOB before updating the DB.
func runStopLoss(ctx context.Context, cfg *config.Config, database *db.DB, scanner *market.Scanner, exec executor.Executor, n *notify.Notifier) {
	// The stop is now per-sport: each check below resolves the drop for that
	// trade's sport and skips (holds) if it's <= 0. No global early-return.

	// ── Paper stop loss ───────────────────────────────────────────────────────
	paperTrades, err := database.GetOpenPaperTrades()
	if err != nil {
		log.Printf("[stoploss] error fetching open paper trades: %v", err)
	} else {
		for _, t := range paperTrades {
			select {
			case <-ctx.Done():
				return
			default:
			}
			checkPaperStopLoss(ctx, cfg, database, scanner, n, t)
		}
	}

	// ── Live stop loss (only when live executor is active) ────────────────────
	liveExec, isLive := exec.(*executor.LiveExecutor)
	if !isLive {
		return
	}

	liveTrades, err := database.GetOpenLiveTrades()
	if err != nil {
		log.Printf("[stoploss] error fetching open live trades: %v", err)
		return
	}
	for _, t := range liveTrades {
		select {
		case <-ctx.Done():
			return
		default:
		}
		checkLiveStopLoss(ctx, cfg, database, scanner, liveExec, n, t)
	}
}

func checkPaperStopLoss(ctx context.Context, cfg *config.Config, database *db.DB, scanner *market.Scanner, n *notify.Notifier, t db.OpenPaperTrade) {
	price, open, err := scanner.GetSidePrice(t.ConditionID, t.Side)
	if err != nil {
		log.Printf("[stoploss] price check error %s: %v", t.ConditionID[:12], err)
		return
	}
	if !open {
		return // market settled — let resolveOpenTrades handle it
	}
	drop := effectiveStopLoss(cfg, database, t.Sport)
	if drop <= 0 {
		return // stop-loss disabled for this sport — hold to settlement
	}
	stopThreshold := t.EntryPrice - drop
	if price >= stopThreshold {
		return // still above threshold, hold
	}

	sellProceeds := t.Shares * price
	sellFee := sellProceeds * 0.02
	grossPnl := sellProceeds - t.SizeUSDC
	netPnl := grossPnl - t.BuyFee - sellFee
	netPnlPct := 0.0
	if t.SizeUSDC > 0 {
		netPnlPct = netPnl / t.SizeUSDC
	}
	fullLoss := -(t.SizeUSDC + t.BuyFee)
	saved := netPnl - fullLoss

	log.Printf("[stoploss/paper] ⛔ %s | %s | entry=%.1f¢ stop=%.1f¢ exit=%.1f¢ | P&L: $%.2f | saved: $%.2f",
		t.Sport, t.Side[:min(30, len(t.Side))], t.EntryPrice*100, stopThreshold*100, price*100, netPnl, saved)

	if err := database.StopLossPaperTrade(t.ConditionID, price, sellFee, grossPnl, netPnlPct); err != nil {
		log.Printf("[stoploss/paper] DB update failed %s: %v", t.ConditionID[:12], err)
		return
	}
	n.StopLossTriggered(t.Market, t.Side, t.Sport, t.Slug, price, netPnl, saved)
}

func checkLiveStopLoss(ctx context.Context, cfg *config.Config, database *db.DB, scanner *market.Scanner, liveExec *executor.LiveExecutor, n *notify.Notifier, t db.OpenPaperTrade) {
	price, tokenID, open, err := scanner.GetSidePriceAndToken(t.ConditionID, t.Side)
	if err != nil {
		log.Printf("[stoploss/live] price check error %s: %v", t.ConditionID[:12], err)
		return
	}
	if !open {
		return // settled — let resolveLiveTrades handle it
	}
	drop := effectiveStopLoss(cfg, database, t.Sport)
	if drop <= 0 {
		return // stop-loss disabled for this sport — hold to settlement
	}
	stopThreshold := t.EntryPrice - drop
	if price >= stopThreshold {
		return // still above threshold, hold
	}
	if tokenID == "" {
		log.Printf("[stoploss/live] no token_id for %s — cannot place sell order", t.ConditionID[:12])
		return
	}

	sellProceeds := t.Shares * price
	sellFee := sellProceeds * 0.02
	grossPnl := sellProceeds - t.SizeUSDC
	netPnl := grossPnl - t.BuyFee - sellFee
	netPnlPct := 0.0
	if t.SizeUSDC > 0 {
		netPnlPct = netPnl / t.SizeUSDC
	}
	fullLoss := -(t.SizeUSDC + t.BuyFee)
	saved := netPnl - fullLoss

	// No-liquidity write-off. When a backed favorite loses, its token collapses
	// toward 0 and ALL bids disappear — a FAK sell just kills against an empty
	// book, PlaceSellOrder can't confirm an exit, and the trade is retried every
	// poll cycle (spamming the CLOB/logs) until the market settles minutes later.
	// Below this price there is effectively no liquidity and nothing meaningful to
	// recover, so record the realized loss ONCE and let on-chain settlement confirm
	// $0 — rather than looping. Above it, a real exit may still fill, so we fall
	// through and keep attempting (and retrying on transient failures) as before.
	const noLiquidityFloor = 0.05 // 5¢
	if price <= noLiquidityFloor {
		log.Printf("[stoploss/live] 💀 %s | %s | entry=%.1f¢ collapsed to %.1f¢ — no book liquidity to exit; writing off $%.2f (no further retries)",
			t.Sport, t.Side[:min(30, len(t.Side))], t.EntryPrice*100, price*100, netPnl)
		if err := database.StopLossLiveTrade(t.ConditionID, price, sellFee, grossPnl, netPnlPct); err != nil {
			log.Printf("[stoploss/live] write-off DB update failed %s: %v", t.ConditionID[:12], err)
			return
		}
		n.StopLossTriggered(t.Market, t.Side, t.Sport, t.Slug, price, netPnl, saved)
		return
	}

	log.Printf("[stoploss/live] ⛔ %s | %s | entry=%.1f¢ stop=%.1f¢ exit=%.1f¢ | P&L: $%.2f | saved: $%.2f",
		t.Sport, t.Side[:min(30, len(t.Side))], t.EntryPrice*100, stopThreshold*100, price*100, netPnl, saved)

	// Resolve which CTF Exchange this token belongs to before signing the sell.
	negRisk, err := scanner.GetNegRisk(tokenID)
	if err != nil {
		log.Printf("[stoploss/live] neg_risk lookup failed %s: %v — defaulting to false", t.ConditionID[:12], err)
		negRisk = false
	}

	// Place actual SELL on the CLOB before updating the DB.
	if err := liveExec.PlaceSellOrder(ctx, t.ConditionID, tokenID, t.Side, t.Shares, price, negRisk); err != nil {
		log.Printf("[stoploss/live] CLOB sell failed %s: %v", t.ConditionID[:12], err)
		return // don't update DB if the sell didn't go through
	}

	if err := database.StopLossLiveTrade(t.ConditionID, price, sellFee, grossPnl, netPnlPct); err != nil {
		log.Printf("[stoploss/live] DB update failed %s: %v", t.ConditionID[:12], err)
		return
	}
	n.StopLossTriggered(t.Market, t.Side, t.Sport, t.Slug, price, netPnl, saved)
}

// resolveLiveTrades checks all open real (non-paper) trades and resolves those
// whose markets have settled. Mirrors resolveOpenTrades for live positions so
// that Telegram/Discord notifications fire on win/loss just like paper trades.
// sync.js will also update the DB on its hourly run — ResolveLiveTrade only
// updates WHERE outcome='NA', so whichever runs first wins without conflict.
func resolveLiveTrades(ctx context.Context, database *db.DB, scanner *market.Scanner, n *notify.Notifier) {
	trades, err := database.GetOpenLiveTrades()
	if err != nil {
		log.Printf("[resolve/live] error fetching open trades: %v", err)
		return
	}
	if len(trades) == 0 {
		return
	}

	log.Printf("[resolve/live] checking %d open live trade(s)", len(trades))
	resolved := 0

	for _, t := range trades {
		select {
		case <-ctx.Done():
			return
		default:
		}

		m, err := scanner.CheckMarket(t.ConditionID)
		if err != nil {
			log.Printf("[resolve/live] skip %s: %v", t.ConditionID[:12], err)
			continue
		}

		effectivelySettled := false
		for _, tok := range m.Tokens {
			if tok.Price >= 0.999 {
				effectivelySettled = true
				break
			}
		}
		if m.Active && !m.Closed && !effectivelySettled {
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

		grossPnl := t.Shares*exitPrice - t.SizeUSDC
		netPnl := grossPnl - t.BuyFee
		netPnlPct := 0.0
		if t.SizeUSDC > 0 {
			netPnlPct = netPnl / t.SizeUSDC
		}

		affected, err := database.ResolveLiveTrade(t.ConditionID, outcome, exitPrice, grossPnl, netPnlPct)
		if err != nil {
			log.Printf("[resolve/live] DB update failed for %s: %v", t.ConditionID[:12], err)
			continue
		}
		if affected == 0 {
			log.Printf("[resolve/live] skip %s — already resolved", t.ConditionID[:12])
			continue
		}

		log.Printf("[resolve/live] %s | %-30s | %s | P&L: $%.2f (%.1f%%)",
			outcome, t.Side, t.ConditionID[:12], netPnl, netPnlPct*100)
		n.TradeResolved(m.Question, t.Side, outcome, m.MarketSlug, netPnl)
		resolved++
	}

	if resolved > 0 {
		log.Printf("[resolve/live] %d trade(s) settled", resolved)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── Telegram command handler ──────────────────────────────────────────────────

// makeCmdHandler returns a CommandHandler that processes Telegram slash commands.
// Supported commands:
//
//	/status        — current mode, circuit breaker, stop-loss, positions, P&L
//	/clearbreaker  — clear an active circuit breaker immediately
//	/bankroll <n>  — set the bankroll used for Kelly sizing
//	/stoploss <c>  — set the stop-loss drop in cents (e.g. /stoploss 25)
//	/fallback <n>  — set the fallback trade size in dollars (e.g. /fallback 50)
//	/stop          — graceful shutdown (pm2 will restart in paper mode)
//	/live          — switch to live trading on next restart
//	/paper         — switch to paper trading on next restart
func makeCmdHandler(cfg *config.Config, database *db.DB, n *notify.Notifier) notify.CommandHandler {
	selfSignal := func() {
		p, err := os.FindProcess(os.Getpid())
		if err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}

	return func(cmd, args string) {
		switch cmd {

		case "status":
			mode := "PAPER"
			if !cfg.DryRun {
				mode = "LIVE"
			}
			breaker, _ := database.GetSetting("circuit_breaker_until")
			breakerMsg := "none"
			if breaker != "" {
				breakerMsg = "ACTIVE until " + breaker
			}
			override, _ := database.GetSetting("mode_override")
			if override == "" {
				override = "none (using DRY_RUN env)"
			}
			paperTrades, _ := database.GetOpenPaperTrades()
			liveTrades, _ := database.GetOpenLiveTrades()
			todayPnL, _ := database.GetTodayPnL()
			allPnL, _ := database.GetAllTimePnL()
			bankroll, since, _ := database.GetBankroll()
			livePnLSince, _ := database.GetLivePnLSince(since)
			balance := bankroll + livePnLSince
			balanceLabel := "Balance (ledger)"
			// Prefer the real on-chain balance the poll loop cached, when available.
			if lb, _ := database.GetSetting("last_balance"); lb != "" {
				if v, perr := strconv.ParseFloat(lb, 64); perr == nil {
					balance = v
					balanceLabel = "Balance (on-chain)"
				}
			}
			stopDrop := effectiveStopLoss(cfg, database, "")
			// Note any per-sport stop-loss overrides (e.g. Tennis disabled).
			stopNote := ""
			if len(cfg.StopLossDropBySport) > 0 {
				var parts []string
				for _, s := range []string{"Tennis", "Baseball", "Hockey", "Basketball", "Soccer"} {
					if d, ok := cfg.StopLossDropBySport[s]; ok {
						if d <= 0 {
							parts = append(parts, s+": off")
						} else {
							parts = append(parts, fmt.Sprintf("%s: %.0f¢", s, d*100))
						}
					}
				}
				if len(parts) > 0 {
					stopNote = " [" + strings.Join(parts, ", ") + "]"
				}
			}

			// Trading-halt status: surface the daily-loss-limit halt and the
			// /startup safety override, which are separate from the circuit breaker.
			today := time.Now().UTC().Format("2006-01-02")
			overrideDate, _ := database.GetSetting("safety_override")
			tradingMsg := "active"
			if killed, _ := database.GetSetting("bot_killed"); killed == "true" {
				tradingMsg = "DORMANT (halted — send /startup to resume)"
			} else if overrideDate == today {
				tradingMsg = "active (safety override until midnight UTC)"
			} else if breaker != "" {
				tradingMsg = "HALTED (circuit breaker)"
			} else if todayPnL < -cfg.MaxDailyLoss {
				tradingMsg = fmt.Sprintf("HALTED (daily loss limit -$%.0f) — resumes midnight UTC", cfg.MaxDailyLoss)
			}

			streakLen, streakKind, _ := database.CurrentStreak()
			streakMsg := "n/a"
			if streakLen > 0 {
				emoji := "✅"
				label := "win"
				if streakKind == "LOSS" {
					emoji = "🔴"
					label = "loss"
				}
				if streakLen != 1 {
					label = label + "s"
				}
				streakMsg = fmt.Sprintf("%s %d %s", emoji, streakLen, label)
			}

			n.Broadcast(fmt.Sprintf(
				"📊 BOT STATUS\n"+
					"Mode: %s (override: %s)\n"+
					"Trading: %s\n"+
					"Circuit breaker: %s\n"+
					"Stop-loss: %.0f¢ drop%s\n"+
					"Fallback size: $%.2f\n"+
					"Open paper trades: %d\n"+
					"Open live trades: %d\n"+
					"Current streak: %s\n"+
					"Today P&L: $%.2f (daily limit -$%.0f)\n"+
					"All-time P&L: $%.2f\n"+
					"Bankroll: $%.2f | %s: $%.2f",
				mode, override,
				tradingMsg,
				breakerMsg,
				stopDrop*100, stopNote,
				effectiveFallback(cfg, database),
				len(paperTrades),
				len(liveTrades),
				streakMsg,
				todayPnL, cfg.MaxDailyLoss,
				allPnL,
				bankroll, balanceLabel, balance,
			))

		case "clearbreaker":
			if err := database.SetSetting("circuit_breaker_until", ""); err != nil {
				n.Broadcast("❌ Failed to clear circuit breaker: " + err.Error())
				return
			}
			log.Println("[cmd] circuit breaker cleared via Telegram")
			n.Broadcast("✅ Circuit breaker cleared — trading resumed.")

		case "bankroll":
			amount, err := strconv.ParseFloat(args, 64)
			if err != nil || amount <= 0 {
				n.Broadcast("❌ Usage: /bankroll <amount>  e.g. /bankroll 1500")
				return
			}
			old, _, _ := database.GetBankroll()
			if err := database.SetSetting("bankroll", fmt.Sprintf("%.2f", amount)); err != nil {
				n.Broadcast("❌ Failed to update bankroll: " + err.Error())
				return
			}
			floor := amount * cfg.BankrollFloorPct
			log.Printf("[cmd] bankroll updated $%.2f → $%.2f via Telegram", old, amount)
			n.Broadcast(fmt.Sprintf(
				"💰 Bankroll updated: $%.2f → $%.2f\nFloor (%.0f%%): $%.2f\nBalance resets to $%.2f — only new live trades will adjust it.",
				old, amount, cfg.BankrollFloorPct*100, floor, amount,
			))

		case "stoploss":
			v, err := strconv.ParseFloat(args, 64)
			if err != nil || v <= 0 {
				n.Broadcast("❌ Usage: /stoploss <cents>  e.g. /stoploss 25  (or /stoploss 0.25)")
				return
			}
			// Accept cents (≥1, e.g. 25 → 0.25) or a fraction (<1, e.g. 0.25).
			drop := v
			if v >= 1 {
				drop = v / 100
			}
			if drop <= 0 || drop >= 1 {
				n.Broadcast("❌ Stop-loss must be between 1¢ and 99¢.")
				return
			}
			oldDrop := effectiveStopLoss(cfg, database, "")
			if err := database.SetSetting("stop_loss_drop", fmt.Sprintf("%.4f", drop)); err != nil {
				n.Broadcast("❌ Failed to update stop-loss: " + err.Error())
				return
			}
			perSportNote := ""
			if len(cfg.StopLossDropBySport) > 0 {
				perSportNote = "\n(Note: per-sport overrides set via env still apply and are unaffected — see /status.)"
			}
			log.Printf("[cmd] stop-loss drop %.0f¢ → %.0f¢ via Telegram", oldDrop*100, drop*100)
			n.Broadcast(fmt.Sprintf(
				"🛡 Stop-loss (global default) updated: %.0f¢ → %.0f¢ drop.\nA 95¢ entry now exits at %.0f¢. Takes effect on the next stop-loss check (no restart).%s",
				oldDrop*100, drop*100, (0.95-drop)*100, perSportNote,
			))

		case "fallback":
			v, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(args), "$"), 64)
			if err != nil || v <= 0 {
				n.Broadcast("❌ Usage: /fallback <dollars>  e.g. /fallback 50")
				return
			}
			if v > cfg.MaxPositionSize {
				n.Broadcast(fmt.Sprintf("❌ Fallback $%.0f exceeds the max position size $%.0f. Lower the fallback or raise the cap first.", v, cfg.MaxPositionSize))
				return
			}
			oldFb := effectiveFallback(cfg, database)
			if err := database.SetSetting("fallback_size", fmt.Sprintf("%.2f", v)); err != nil {
				n.Broadcast("❌ Failed to update fallback: " + err.Error())
				return
			}
			log.Printf("[cmd] fallback size $%.2f → $%.2f via Telegram", oldFb, v)
			n.Broadcast(fmt.Sprintf(
				"💵 Fallback trade size updated: $%.2f → $%.2f.\nUsed when Kelly can't size a trade. Takes effect on the next trade (no restart).",
				oldFb, v,
			))

		case "kill":
			// Hard halt: set the killed flag then restart via SIGTERM.
			// pm2 will bring the process back up, but it will see the flag
			// and enter halted mode (Telegram listener only — no trading).
			// Resume with /startup or by SSH-ing in and running:
			//   sqlite3 /home/ubuntu/app/trades.db "DELETE FROM settings WHERE key='bot_killed';"
			//   pm2 restart polymarket-bot
			if err := database.SetSetting("bot_killed", "true"); err != nil {
				n.Broadcast("❌ Failed to set kill switch: " + err.Error())
				return
			}
			n.Broadcast("🔴 KILL SWITCH ACTIVATED — trading halted.\npm2 will keep the process alive but dormant.\nSend /startup to resume trading.")
			log.Println("[cmd] kill switch activated via Telegram — entering halted mode")
			selfSignal()

		case "startup":
			// Resume trading: clear the kill switch and circuit breaker, and set a
			// same-day safety override so the circuit breaker and daily loss limit
			// don't immediately re-engage on the existing loss streak / P&L. The
			// override (and the daily loss halt) reset at midnight UTC. The bankroll
			// floor is NOT overridden — it remains a hard stop.
			today := time.Now().UTC().Format("2006-01-02")
			_ = database.SetSetting("bot_killed", "")
			_ = database.SetSetting("circuit_breaker_until", "")
			_ = database.SetSetting("daily_loss_halt", "")
			_ = database.SetSetting("safety_override", today)
			n.Broadcast(
				"🟢 Trading resumed — kill switch, circuit breaker, and daily loss halt cleared.\n" +
					"⚠️ Circuit breaker and daily loss limit are OVERRIDDEN until midnight UTC. Bankroll floor still applies.")
			log.Println("[cmd] startup — cleared kill switch + circuit breaker + daily-loss halt; safety override until midnight UTC")
			selfSignal()

		case "stop":
			n.Broadcast("🛑 Bot stopping on Telegram command. pm2 will restart it in paper mode.")
			log.Println("[cmd] stop requested via Telegram")
			selfSignal()

		case "live":
			// Sanity-check credentials before committing to live mode so the
			// bot doesn't enter a crash loop if the keys are invalid.
			if cfg.PolyPrivateKey == "" || cfg.PolyAPIKey == "" {
				n.Broadcast("❌ Cannot switch to LIVE — POLY_PRIVATE_KEY / POLY_API_KEY not set in config.")
				return
			}
			liveExec, err := executor.NewLive(
				cfg.PolyPrivateKey, cfg.PolyAPIKey,
				cfg.PolyAPISecret, cfg.PolyAPIPassphrase,
				cfg.PolyProxyWallet,
				database,
			)
			if err != nil {
				n.Broadcast("❌ Live executor init failed: " + err.Error())
				return
			}
			if err := liveExec.VerifyCredentials(context.Background()); err != nil {
				n.Broadcast("❌ CLOB credential check failed — keys may be expired.\nError: " + err.Error() + "\n\nRegenerate keys at polymarket.com and update ecosystem.config.cjs.")
				return
			}
			if err := database.SetSetting("mode_override", "live"); err != nil {
				n.Broadcast("❌ Failed to set live mode: " + err.Error())
				return
			}
			n.Broadcast("🟢 Credentials verified — switching to LIVE mode, restarting now...")
			log.Println("[cmd] switching to LIVE mode via Telegram")
			selfSignal()

		case "paper":
			if err := database.SetSetting("mode_override", "paper"); err != nil {
				n.Broadcast("❌ Failed to set paper mode: " + err.Error())
				return
			}
			n.Broadcast("📝 Switching to PAPER mode — restarting now...")
			log.Println("[cmd] switching to PAPER mode via Telegram")
			selfSignal()

		case "help":
			n.Broadcast(helpText())

		default:
			n.Broadcast(fmt.Sprintf("❓ Unknown command: /%s\n\n%s", cmd, helpText()))
		}
	}
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

		// A market is considered resolvable when it is officially closed OR
		// when one token has reached ≥99.9¢ (effectively settled — Polymarket
		// sometimes takes days to officially close a market after the event).
		effectivelySettled := false
		for _, tok := range m.Tokens {
			if tok.Price >= 0.999 {
				effectivelySettled = true
				break
			}
		}
		if m.Active && !m.Closed && !effectivelySettled {
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

		grossPnl := t.Shares*exitPrice - t.SizeUSDC
		netPnl := grossPnl - t.BuyFee
		netPnlPct := 0.0
		if t.SizeUSDC > 0 {
			netPnlPct = netPnl / t.SizeUSDC
		}

		affected, err := database.ResolvePaperTrade(t.ConditionID, outcome, exitPrice, grossPnl, netPnlPct)
		if err != nil {
			log.Printf("[resolve] DB update failed for %s: %v", t.ConditionID[:12], err)
			continue
		}
		if affected == 0 {
			// Trade was already resolved by a prior run — skip notification to prevent duplicates.
			log.Printf("[resolve] skip %s — already resolved (0 rows affected)", t.ConditionID[:12])
			continue
		}

		log.Printf("[resolve] %s | %-30s | %s | P&L: $%.2f (%.1f%%) [fee: $%.2f]",
			outcome, t.Side, t.ConditionID[:12], netPnl, netPnlPct*100, t.BuyFee)
		n.TradeResolved(m.Question, t.Side, outcome, m.MarketSlug, netPnl)
		resolved++
	}

	if resolved > 0 {
		log.Printf("[resolve] %d trade(s) settled", resolved)
	}
}
