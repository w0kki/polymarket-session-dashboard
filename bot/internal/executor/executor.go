package executor

import (
	"context"
	"fmt"
	"log"

	"github.com/w0kki/polymarket-bot/internal/db"
	"github.com/w0kki/polymarket-bot/internal/kelly"
	"github.com/w0kki/polymarket-bot/internal/market"
)

// Executor is the interface every execution mode implements.
// Swapping paper → live is a single config flag — no logic changes needed.
type Executor interface {
	PlaceOrder(ctx context.Context, opp market.Opportunity) error
}

// ── Paper executor ────────────────────────────────────────────────────────────

// PaperExecutor logs trades to the shared SQLite DB without touching
// the Polymarket CLOB. Every paper trade appears in the dashboard
// trade log with trade_type = 'Paper' so you can review decisions.
type PaperExecutor struct {
	db *db.DB
}

func NewPaper(d *db.DB) *PaperExecutor {
	return &PaperExecutor{db: d}
}

func (p *PaperExecutor) PlaceOrder(_ context.Context, opp market.Opportunity) error {
	buyFee := kelly.CalcBuyFee(opp.Shares, opp.Price, opp.Sport)

	trade := db.PaperTrade{
		ConditionID: opp.ConditionID,
		Market:      opp.Market,
		Slug:        opp.Slug,
		Sport:       opp.Sport,
		Side:        opp.Side,
		EntryPrice:  opp.Price,
		Shares:      opp.Shares,
		SizeUSDC:    opp.SizeUSDC,
		BuyFee:      buyFee,
	}

	if err := p.db.InsertPaperTrade(trade); err != nil {
		return fmt.Errorf("paper executor: %w", err)
	}

	log.Printf("[PAPER] %-10s | %-50s | side=%-30s | price=%.2f¢ | shares=%.2f | size=$%.2f | fee=$%.4f",
		opp.Sport, truncate(opp.Market, 50), opp.Side,
		opp.Price*100, opp.Shares, opp.SizeUSDC, buyFee,
	)
	return nil
}

// ── Live executor stub ────────────────────────────────────────────────────────

// LiveExecutor will be implemented in Phase 8 (EIP-712 signing + CLOB API).
// It panics now so it cannot be accidentally activated during paper testing.
type LiveExecutor struct{}

func NewLive() *LiveExecutor { return &LiveExecutor{} }

func (l *LiveExecutor) PlaceOrder(_ context.Context, opp market.Opportunity) error {
	panic("LiveExecutor not implemented — set DRY_RUN=true for paper trading")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
