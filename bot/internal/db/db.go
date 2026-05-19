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
func (d *DB) GetBankroll() (float64, error) {
	var raw string
	err := d.conn.QueryRow(`SELECT value FROM settings WHERE key = 'bankroll'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get bankroll: %w", err)
	}
	var v float64
	fmt.Sscanf(raw, "%f", &v)
	return v, nil
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
			SUM(CASE WHEN outcome = 'WIN'  THEN 1 ELSE 0 END),
			SUM(CASE WHEN outcome = 'LOSS' THEN 1 ELSE 0 END),
			AVG(CASE WHEN outcome = 'WIN'  AND pnl IS NOT NULL THEN pnl      END),
			AVG(CASE WHEN outcome = 'LOSS' AND pnl IS NOT NULL THEN ABS(pnl) END)
		FROM trades
		WHERE outcome IN ('WIN', 'LOSS')
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
