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

// Compute calculates the Kelly fraction from historical trade stats.
//
//   b = avgWin / avgLoss   (win-to-loss ratio)
//   p = wins / (wins+losses)
//   q = 1 − p
//   f* = (b·p − q) / b
//
// Returns Computed=false and falls back to fallbackSize when there are
// fewer than 1 loss (b cannot be computed without a loss).
func Compute(wins, losses int, avgWin, avgLoss, bankroll, maxSize, fallbackSize float64) Result {
	if losses < 1 || avgLoss <= 0 || avgWin <= 0 {
		// Not enough loss data yet — use the configured fallback size.
		size := math.Min(fallbackSize, maxSize)
		return Result{PositionSize: size, Computed: false}
	}

	b := avgWin / avgLoss
	p := float64(wins) / float64(wins+losses)
	q := 1 - p

	full := (b*p - q) / b
	if full <= 0 {
		// Negative Kelly means no edge — don't bet.
		return Result{FullKelly: 0, HalfKelly: 0, PositionSize: 0, Computed: true}
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

// CalcBuyFee computes the Polymarket buy fee for a position.
//
//	fee = shares × price × feeRate × price × (1 − price)
func CalcBuyFee(shares, price float64, sport string) float64 {
	r := FeeRate(sport)
	return shares * price * r * price * (1 - price)
}
