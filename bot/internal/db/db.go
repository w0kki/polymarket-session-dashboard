package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the shared trades.db used by both the dashboard and the bot.
type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	log.Printf("[db] connected to %s", path)
	return &DB{conn: conn}, nil
}

func (d *DB) Close() error { return d.conn.Close() }

// ── Settings ─────────────────────────────────────────────────────────────────

// GetBankroll reads the bankroll from the settings table (set via KCC panel).
// Also returns the UTC timestamp when the bankroll was last set, used to
// compute balance = bankroll + live P&L since that date.
// timestamp is empty string when the row has never been written with a timestamp.
func (d *DB) GetBankroll() (amount float64, since string, err error) {
	var raw, ts string
	e := d.conn.QueryRow(`SELECT value, updated_at FROM settings WHERE key = 'bankroll'`).Scan(&raw, &ts)
	if e == sql.ErrNoRows {
		return 0, "", nil
	}
	if e != nil {
		return 0, "", fmt.Errorf("get bankroll: %w", e)
	}
	fmt.Sscanf(raw, "%f", &amount)
	return amount, ts, nil
}

// ── Trade stats (for Kelly) ──────────────────────────────────────────────────

// TradeStats contains the data needed to compute the Kelly fraction.
type TradeStats struct {
	Wins    int
	Losses  int
	AvgWin  float64 // average $ P&L on winning trades
	AvgLoss float64 // average $ loss magnitude on losing trades (positive)
}

func (d *DB) GetTradeStats() (*TradeStats, error) {
	row := d.conn.QueryRow(`
		SELECT
			SUM(CASE WHEN outcome = 'WIN'                        THEN 1 ELSE 0 END),
			SUM(CASE WHEN outcome IN ('LOSS','STOP_LOSS')        THEN 1 ELSE 0 END),
			AVG(CASE WHEN outcome = 'WIN'                 AND net_pnl IS NOT NULL THEN net_pnl        END),
			AVG(CASE WHEN outcome IN ('LOSS','STOP_LOSS') AND net_pnl IS NOT NULL THEN ABS(net_pnl)   END)
		FROM trades
		WHERE outcome IN ('WIN', 'LOSS', 'STOP_LOSS')
		  AND trade_type IN ('Risk Premia', 'Latency Arb')
	`)

	var wins, losses sql.NullInt64
	var avgWin, avgLoss sql.NullFloat64
	if err := row.Scan(&wins, &losses, &avgWin, &avgLoss); err != nil {
		return nil, fmt.Errorf("get trade stats: %w", err)
	}
	return &TradeStats{
		Wins:    int(wins.Int64),
		Losses:  int(losses.Int64),
		AvgWin:  avgWin.Float64,
		AvgLoss: avgLoss.Float64,
	}, nil
}

// CurrentStreak walks the most recent SETTLED live trades (newest first) and
// counts how many in a row share the same outcome category (WIN vs LOSS/
// STOP_LOSS). Returns the length and the kind ("WIN" or "LOSS"). If no settled
// trades exist returns (0, "", nil).
func (d *DB) CurrentStreak() (length int, kind string, err error) {
	rows, qerr := d.conn.Query(`
		SELECT outcome FROM trades
		WHERE trade_type IN ('Risk Premia', 'Latency Arb')
		  AND outcome IN ('WIN', 'LOSS', 'STOP_LOSS')
		ORDER BY updated_at DESC
		LIMIT 500
	`)
	if qerr != nil {
		return 0, "", fmt.Errorf("current streak: %w", qerr)
	}
	defer rows.Close()

	for rows.Next() {
		var outcome string
		if scanErr := rows.Scan(&outcome); scanErr != nil {
			return length, kind, scanErr
		}
		var thisKind string
		if outcome == "WIN" {
			thisKind = "WIN"
		} else {
			thisKind = "LOSS"
		}
		if kind == "" {
			kind = thisKind
			length = 1
			continue
		}
		if thisKind != kind {
			break
		}
		length++
	}
	return length, kind, nil
}

// ── Deduplication ────────────────────────────────────────────────────────────

// IsAlreadyTraded returns true if a conditionId already exists in the trades
// table — covers both real trades (synced from wallet) and paper trades.
func (d *DB) IsAlreadyTraded(conditionID string) (bool, error) {
	var n int
	err := d.conn.QueryRow(
		`SELECT COUNT(*) FROM trades WHERE condition_id = ?`, conditionID,
	).Scan(&n)
	return n > 0, err
}

// ActiveConditionIDs returns the set of conditionIds currently in the
// positions table (synced from wallet). Used to skip markets already held.
func (d *DB) ActiveConditionIDs() (map[string]bool, error) {
	rows, err := d.conn.Query(`SELECT condition_id FROM positions`)
	if err != nil {
		return nil, fmt.Errorf("active positions: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var cid string
		if err := rows.Scan(&cid); err != nil {
			return nil, err
		}
		out[cid] = true
	}
	return out, rows.Err()
}

// ── Safety net queries ───────────────────────────────────────────────────────

// GetAllTimePnL returns total net P&L across all resolved trades (paper + live).
// Used for display purposes only (dashboard, /status).
func (d *DB) GetAllTimePnL() (float64, error) {
	var pnl float64
	err := d.conn.QueryRow(`
		SELECT COALESCE(SUM(net_pnl), 0)
		FROM trades
		WHERE outcome IN ('WIN', 'LOSS', 'STOP_LOSS')
		  AND net_pnl IS NOT NULL
	`).Scan(&pnl)
	return pnl, err
}

// GetLivePnLSince returns net P&L from real (non-paper) trades resolved
// after the given UTC timestamp string (SQLite datetime format).
// Used to compute the current effective balance: bankroll + GetLivePnLSince(since).
// Only counting trades after the bankroll was last set prevents double-counting
// losses that already occurred before the bankroll figure was updated.
func (d *DB) GetLivePnLSince(since string) (float64, error) {
	var pnl float64
	err := d.conn.QueryRow(`
		SELECT COALESCE(SUM(net_pnl), 0)
		FROM trades
		WHERE outcome IN ('WIN', 'LOSS', 'STOP_LOSS')
		  AND net_pnl IS NOT NULL
		  AND trade_type IN ('Risk Premia', 'Latency Arb')
		  AND updated_at >= ?
	`, since).Scan(&pnl)
	return pnl, err
}

// GetLiveAllTimePnL returns total net P&L from real (non-paper) trades only.
// Used for display in /status (all-time figure, not balance-adjusted).
func (d *DB) GetLiveAllTimePnL() (float64, error) {
	var pnl float64
	err := d.conn.QueryRow(`
		SELECT COALESCE(SUM(net_pnl), 0)
		FROM trades
		WHERE outcome IN ('WIN', 'LOSS', 'STOP_LOSS')
		  AND net_pnl IS NOT NULL
		  AND trade_type IN ('Risk Premia', 'Latency Arb')
	`).Scan(&pnl)
	return pnl, err
}

// GetTodayPnL returns the sum of live P&L RESOLVED today (UTC) — the figure the
// daily-loss limit checks against. Keyed on the resolution timestamp (updated_at),
// not the entry date: a position entered late yesterday that loses after midnight
// is a loss that happened today and must count toward today's limit. (The old
// entry-date basis let cross-midnight losses slip past the limit entirely.)
// Live trade types only — paper outcomes must not trip the live halt.
func (d *DB) GetTodayPnL() (float64, error) {
	var pnl float64
	err := d.conn.QueryRow(`
		SELECT COALESCE(SUM(net_pnl), 0)
		FROM trades
		WHERE date(updated_at) = date('now')
		  AND outcome IN ('WIN', 'LOSS', 'STOP_LOSS')
		  AND net_pnl IS NOT NULL
		  AND trade_type IN ('Risk Premia', 'Latency Arb')
	`).Scan(&pnl)
	return pnl, err
}

// GetConsecutiveLosses returns the number of consecutive LOSS outcomes
// at the tail of the live trade history (ordered by updated_at desc).
// Only live trades (Risk Premia, Latency Arb) are considered — paper trade
// outcomes should not trigger the live circuit breaker.
func (d *DB) GetConsecutiveLosses() (int, error) {
	rows, err := d.conn.Query(`
		SELECT outcome FROM trades
		WHERE outcome IN ('WIN', 'LOSS', 'STOP_LOSS')
		  AND trade_type IN ('Risk Premia', 'Latency Arb')
		ORDER BY updated_at DESC
		LIMIT 20
	`)
	if err != nil {
		return 0, fmt.Errorf("get consecutive losses: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			return 0, err
		}
		if outcome == "WIN" {
			break // a win resets the streak
		}
		count++ // LOSS and STOP_LOSS both count toward the streak
	}
	return count, rows.Err()
}

// GetSetting reads an arbitrary value from the settings table.
// Returns "" with no error when the key does not exist.
func (d *DB) GetSetting(key string) (string, error) {
	var val string
	err := d.conn.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

// SetSetting upserts a key-value pair in the settings table.
func (d *DB) SetSetting(key, value string) error {
	// Update updated_at too: GetBankroll uses the bankroll row's timestamp as
	// the "since" baseline for the balance calc, so re-setting the bankroll must
	// re-baseline the date (otherwise prior P&L is double-counted and the
	// balance never resets to the new figure). Harmless for other keys.
	_, err := d.conn.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = datetime('now')
	`, key, value)
	return err
}

// ── Live sports state (written by sports_collector.py sidecar) ───────────────

// LiveSport is the current live state of a game from Polymarket's sports feed.
type LiveSport struct {
	GameID    int
	Sport     string // "tennis", "basketball", etc.
	Period    string // tennis: "S1".."S5", "TB1".."TB5"; null/"" if unknown
	Score     string // e.g. "6-4, 3-6, 5-4" or "2-1"
	Live      bool
	Ended     bool
	UpdatedAt string // UTC "2006-01-02 15:04:05"
}

// GetLiveSport returns the live state for a gameId, or (nil, nil) if the
// sidecar has no row for it yet. Missing table is treated as no data.
func (d *DB) GetLiveSport(gameID int) (*LiveSport, error) {
	var s LiveSport
	var live, ended int
	err := d.conn.QueryRow(`
		SELECT game_id, sport, period, score, live, ended, updated_at
		FROM live_sports WHERE game_id = ?
	`, gameID).Scan(&s.GameID, &s.Sport, &s.Period, &s.Score, &live, &ended, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		// Table may not exist yet if the sidecar hasn't started — treat as no data.
		return nil, nil
	}
	s.Live = live == 1
	s.Ended = ended == 1
	return &s, nil
}

// GetLiveSportOfficial returns the official-feed state for a game matched by
// sport + team names (case-insensitive). Returns (nil, nil) if no match is
// found or the official-feed table doesn't exist. Used to gate trades on the
// canonical league feed (MLB Stats / NHL / NBA) instead of Polymarket's
// laggier sports feed.
//
// The official feed score is "away-home" while Polymarket's is "home-away" —
// this returns the score AS-RECORDED-IN-OFFICIAL (away-home). Callers should
// be aware when computing run differentials (abs() is fine since it's symmetric).
func (d *DB) GetLiveSportOfficial(sport, homeTeam, awayTeam string) (*LiveSport, error) {
	var s LiveSport
	var live, ended int
	err := d.conn.QueryRow(`
		SELECT 0 as game_id, sport, period, score, live, ended, updated_at
		FROM live_sports_official
		WHERE LOWER(sport) = LOWER(?)
		  AND LOWER(home_team) = LOWER(?)
		  AND LOWER(away_team) = LOWER(?)
		ORDER BY updated_at DESC LIMIT 1
	`, sport, homeTeam, awayTeam).Scan(&s.GameID, &s.Sport, &s.Period, &s.Score, &live, &ended, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		// Table may not exist yet — treat as no data.
		return nil, nil
	}
	s.Live = live == 1
	s.Ended = ended == 1
	return &s, nil
}

// ── Paper trade writes ───────────────────────────────────────────────────────

// PaperTrade is what the bot inserts when DRY_RUN=true.
type PaperTrade struct {
	ConditionID string
	Date        string
	Market      string
	Slug        string
	Sport       string
	Side        string
	EntryPrice  float64
	Shares      float64
	SizeUSDC    float64
	BuyFee      float64
}

// OpenPaperTrade is a paper trade that has not yet been resolved.
type OpenPaperTrade struct {
	ConditionID string
	Market      string  // human-readable title
	Slug        string  // Polymarket event slug — used to build clickable links
	Sport       string
	Side        string  // outcome label the bot bet on
	EntryPrice  float64
	Shares      float64
	SizeUSDC    float64 // amount paid (cost basis)
	BuyFee      float64
}

// GetOpenPaperTrades returns all paper trades still awaiting resolution.
func (d *DB) GetOpenPaperTrades() ([]OpenPaperTrade, error) {
	rows, err := d.conn.Query(`
		SELECT condition_id, COALESCE(market,''), COALESCE(slug,''),
		       COALESCE(sport,''), side, COALESCE(entry_price,0),
		       shares, size_usdc, COALESCE(buy_fee, 0)
		FROM trades
		WHERE trade_type = 'Paper' AND outcome = 'NA'
	`)
	if err != nil {
		return nil, fmt.Errorf("get open paper trades: %w", err)
	}
	defer rows.Close()

	var out []OpenPaperTrade
	for rows.Next() {
		var t OpenPaperTrade
		if err := rows.Scan(&t.ConditionID, &t.Market, &t.Slug, &t.Sport,
			&t.Side, &t.EntryPrice, &t.Shares, &t.SizeUSDC, &t.BuyFee); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StopLossPaperTrade marks a paper trade as exited early via stop loss.
// Records the partial exit price, sell-side fee, and resulting P&L.
func (d *DB) StopLossPaperTrade(conditionID string, exitPrice, sellFee, grossPnl, netPnlPct float64) error {
	_, err := d.conn.Exec(`
		UPDATE trades SET
			outcome    = 'STOP_LOSS',
			exit_price = ?,
			sell_fee   = ?,
			total_fees = COALESCE(buy_fee, 0) + ?,
			pnl        = ?,
			pnl_pct    = ?,
			net_pnl    = ? - COALESCE(buy_fee, 0) - ?,
			updated_at = datetime('now')
		WHERE condition_id = ? AND trade_type = 'Paper' AND outcome = 'NA'
	`, exitPrice, sellFee, sellFee, grossPnl, netPnlPct, grossPnl, sellFee, conditionID)
	return err
}

// ResolvePaperTrade writes the final outcome, exit price, and P&L for a paper trade.
// sell_fee is 0 for redemptions; net_pnl = pnl − buy_fee (already stored).
// Returns the number of rows updated (0 means the trade was already resolved — caller
// must NOT send a notification in that case, or it will fire repeatedly).
func (d *DB) ResolvePaperTrade(conditionID, outcome string, exitPrice, pnl, pnlPct float64) (int64, error) {
	result, err := d.conn.Exec(`
		UPDATE trades SET
			outcome    = ?,
			exit_price = ?,
			pnl        = ?,
			pnl_pct    = ?,
			net_pnl    = ? - COALESCE(buy_fee, 0),
			updated_at = datetime('now')
		WHERE condition_id = ? AND trade_type = 'Paper' AND outcome = 'NA'
	`, outcome, exitPrice, pnl, pnlPct, pnl, conditionID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// InsertPaperTrade writes a paper trade to the shared trades table.
// Uses ON CONFLICT DO NOTHING so re-runs are idempotent.
// trade_type = 'Paper' distinguishes these from real trades in the dashboard.
func (d *DB) InsertPaperTrade(t PaperTrade) error {
	if t.Date == "" {
		t.Date = time.Now().UTC().Format("2006-01-02")
	}
	_, err := d.conn.Exec(`
		INSERT INTO trades (
			condition_id, date, market, slug, sport, trade_type, side,
			entry_price, shares, size_usdc, outcome,
			buy_fee, sell_fee, total_fees, updated_at
		) VALUES (?, ?, ?, ?, ?, 'Paper', ?, ?, ?, ?, 'NA', ?, 0, ?, datetime('now'))
		ON CONFLICT(condition_id) DO NOTHING
	`,
		t.ConditionID, t.Date, t.Market, t.Slug, t.Sport,
		t.Side, t.EntryPrice, t.Shares, t.SizeUSDC,
		t.BuyFee, t.BuyFee, // total_fees = buy_fee (no sell fee yet)
	)
	if err != nil {
		return fmt.Errorf("insert paper trade: %w", err)
	}
	return nil
}

// ── Live trade writes ─────────────────────────────────────────────────────────

// InsertLiveTrade writes a real (CLOB-executed) trade to the DB immediately
// after the order is placed. This ensures dedup, stop-loss monitoring, and
// Kelly sizing all work without waiting for the hourly sync.js run.
// trade_type mirrors what sync.js will write ('Risk Premia' or 'Latency Arb').
func (d *DB) InsertLiveTrade(t PaperTrade) error {
	if t.Date == "" {
		t.Date = time.Now().UTC().Format("2006-01-02")
	}
	tradeType := "Risk Premia"
	if t.EntryPrice < 0.5 {
		tradeType = "Latency Arb"
	}
	_, err := d.conn.Exec(`
		INSERT INTO trades (
			condition_id, date, market, slug, sport, trade_type, side,
			entry_price, shares, size_usdc, outcome,
			buy_fee, sell_fee, total_fees, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'NA', ?, 0, ?, datetime('now'))
		ON CONFLICT(condition_id) DO NOTHING
	`,
		t.ConditionID, t.Date, t.Market, t.Slug, t.Sport, tradeType,
		t.Side, t.EntryPrice, t.Shares, t.SizeUSDC,
		t.BuyFee, t.BuyFee,
	)
	if err != nil {
		return fmt.Errorf("insert live trade: %w", err)
	}
	return nil
}

// GetOpenLiveTrades returns all real (non-paper) trades still awaiting resolution.
func (d *DB) GetOpenLiveTrades() ([]OpenPaperTrade, error) {
	rows, err := d.conn.Query(`
		SELECT condition_id, COALESCE(market,''), COALESCE(slug,''),
		       COALESCE(sport,''), side, COALESCE(entry_price,0),
		       shares, size_usdc, COALESCE(buy_fee, 0)
		FROM trades
		WHERE trade_type IN ('Risk Premia', 'Latency Arb') AND outcome = 'NA'
	`)
	if err != nil {
		return nil, fmt.Errorf("get open live trades: %w", err)
	}
	defer rows.Close()

	var out []OpenPaperTrade
	for rows.Next() {
		var t OpenPaperTrade
		if err := rows.Scan(&t.ConditionID, &t.Market, &t.Slug, &t.Sport,
			&t.Side, &t.EntryPrice, &t.Shares, &t.SizeUSDC, &t.BuyFee); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StopLossLiveTrade marks a live trade as exited early via stop loss.
func (d *DB) StopLossLiveTrade(conditionID string, exitPrice, sellFee, grossPnl, netPnlPct float64) error {
	_, err := d.conn.Exec(`
		UPDATE trades SET
			outcome    = 'STOP_LOSS',
			exit_price = ?,
			sell_fee   = ?,
			total_fees = COALESCE(buy_fee, 0) + ?,
			pnl        = ?,
			pnl_pct    = ?,
			net_pnl    = ? - COALESCE(buy_fee, 0) - ?,
			updated_at = datetime('now')
		WHERE condition_id = ?
		  AND trade_type IN ('Risk Premia', 'Latency Arb')
		  AND outcome = 'NA'
	`, exitPrice, sellFee, sellFee, grossPnl, netPnlPct, grossPnl, sellFee, conditionID)
	return err
}

// ResolveLiveTrade writes the final outcome for a real trade. Returns rows
// affected so the caller can suppress duplicate notifications (0 = already done).
func (d *DB) ResolveLiveTrade(conditionID, outcome string, exitPrice, pnl, pnlPct float64) (int64, error) {
	result, err := d.conn.Exec(`
		UPDATE trades SET
			outcome    = ?,
			exit_price = ?,
			pnl        = ?,
			pnl_pct    = ?,
			net_pnl    = ? - COALESCE(buy_fee, 0),
			updated_at = datetime('now')
		WHERE condition_id = ?
		  AND trade_type IN ('Risk Premia', 'Latency Arb')
		  AND outcome = 'NA'
	`, outcome, exitPrice, pnl, pnlPct, pnl, conditionID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
