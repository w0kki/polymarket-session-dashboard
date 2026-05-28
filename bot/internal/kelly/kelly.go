package kelly

import (
	"math"
)

// Result holds the Kelly outputs for a given scenario.
type Result struct {
	FullKelly    float64 // f* = (b·p − q) / b
	HalfKelly    float64 // recommended: full / 2
	PositionSize float64 // half-Kelly × bankroll, capped at maxSize
	Computed     bool    // false if not enough data to compute
}

// minLossesForKelly is the minimum number of resolved losses required before
// trusting the Kelly computation. Below this threshold the sample variance is
// too high and Kelly will frequently land at zero or negative even for a
// genuinely profitable strategy — so we fall back to the fixed fallback size.
const minLossesForKelly = 20

// Compute calculates the Kelly fraction from historical trade stats.
//
//   b = avgWin / avgLoss   (win-to-loss ratio)
//   p = wins / (wins+losses)
//   q = 1 − p
//   f* = (b·p − q) / b
//
// Returns Computed=false and falls back to fallbackSize when:
//   - fewer than minLossesForKelly losses recorded (insufficient sample)
//   - avgLoss or avgWin are zero/missing
//   - computed Kelly fraction is ≤ 0 (no statistical edge yet)
func Compute(wins, losses int, avgWin, avgLoss, bankroll, maxSize, fallbackSize float64) Result {
	fallback := func() Result {
		size := math.Min(fallbackSize, maxSize)
		return Result{PositionSize: size, Computed: false}
	}

	if losses < minLossesForKelly || avgLoss <= 0 || avgWin <= 0 {
		// Not enough loss data yet — use the configured fallback size.
		return fallback()
	}

	b := avgWin / avgLoss
	p := float64(wins) / float64(wins+losses)
	q := 1 - p

	full := (b*p - q) / b
	if full <= 0 {
		// Negative or zero Kelly: strategy shows no statistical edge yet.
		// Fall back to fixed size rather than halting — the sample may be
		// too small or skewed by outlier losses.
		return fallback()
	}

	half := full / 2
	size := math.Min(half*bankroll, maxSize)

	return Result{
		FullKelly:    full,
		HalfKelly:    half,
		PositionSize: size,
		Computed:     true,
	}
}

// FeeRate returns the Polymarket fee rate for a given sport category.
// Mirrors the feeRate() function in polymarket.ts.
func FeeRate(sport string) float64 {
	switch sport {
	case "Crypto":
		return 0.072
	case "Finance", "Politics", "Tech":
		return 0.04
	case "Culture":
		return 0.05
	case "Economics":
		return 0.03
	case "Weather":
		return 0.025
	case "Mentions":
		return 0.25
	case "Geopolitics":
		return 0
	default:
		return 0.03 // Sports default (Baseball, Soccer, Tennis, etc.)
	}
}

// CalcBuyFee computes the Polymarket CLOB taker fee for a buy order.
// Taker fee = 2% of USDC spent (shares × price).
// The sport parameter is kept for API compatibility but is no longer used —
// CLOB taker fees are flat across all market categories.
func CalcBuyFee(shares, price float64, _ string) float64 {
	const takerFeeRate = 0.02 // 2% CLOB taker fee
	return shares * price * takerFeeRate
}
