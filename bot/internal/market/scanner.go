package market

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// ── Sport detection ───────────────────────────────────────────────────────────

// slugSports maps slug prefixes → sport names.
// Mirrors detectSport() in polymarket.ts — keep in sync.
var slugSports = map[string]string{
	"epl": "Soccer", "lal": "Soccer", "fl1": "Soccer", "fl2": "Soccer",
	"spl": "Soccer", "mls": "Soccer", "bun": "Soccer", "ser": "Soccer",
	"ucl": "Soccer", "uel": "Soccer", "wc": "Soccer", "afc": "Soccer",
	"mlb": "Baseball",
	"nba": "Basketball",
	"nfl": "Football",
	"nhl": "Hockey",
	"atp": "Tennis", "wta": "Tennis",
	"ufc": "MMA",
}

// DetectSport returns the sport name for a market slug, or "" if unknown.
func DetectSport(slug string) string {
	prefix := strings.ToLower(strings.SplitN(slug, "-", 2)[0])
	return slugSports[prefix]
}

// ── Gamma API types ───────────────────────────────────────────────────────────
// Field names verified against gamma-api.polymarket.com.
// outcomePrices and outcomes arrive as JSON-encoded strings, e.g.
//   outcomePrices: "[\"0.95\",\"0.05\"]"
//   outcomes:      "[\"Yes\",\"No\"]"

type gammaMarket struct {
	ConditionID   string `json:"conditionId"`
	Question      string `json:"question"`
	Slug          string `json:"slug"`
	Active        bool   `json:"active"`
	Closed        bool   `json:"closed"`
	EndDateISO    string `json:"endDateIso"`
	OutcomePrices string `json:"outcomePrices"` // JSON string → []string
	Outcomes      string `json:"outcomes"`       // JSON string → []string
	Icon          string `json:"icon"`
}

// Opportunity represents a market that passed all filters and is ready
// to be sized and executed (or paper-logged).
type Opportunity struct {
	ConditionID string
	Market      string  // human-readable title
	Slug        string
	Sport       string
	Side        string  // winning outcome label, e.g. "Arizona Diamondbacks"
	Price       float64 // token price (e.g. 0.94)
	Shares      float64 // position_size / price
	SizeUSDC    float64 // final position size after Kelly + cap
	Icon        string
}

// ── Scanner ───────────────────────────────────────────────────────────────────

type Scanner struct {
	threshold       float64
	sports          []string
	maxSize         float64
	minHoursToClose float64
	client          *http.Client
}

func NewScanner(threshold float64, sports []string, maxSize, minHoursToClose float64) *Scanner {
	return &Scanner{
		threshold:       threshold,
		sports:          sports,
		maxSize:         maxSize,
		minHoursToClose: minHoursToClose,
		client:          &http.Client{Timeout: 20 * time.Second},
	}
}

// Scan fetches active markets and returns those that pass all filters.
//
//   alreadyTraded  – conditionIds already in the trades table (skip)
//   activePositions – conditionIds currently held in the wallet (skip)
//   sizer          – function that returns the desired $ size for a price
func (s *Scanner) Scan(
	alreadyTraded map[string]bool,
	activePositions map[string]bool,
	sizer func(price float64) float64,
) ([]Opportunity, error) {
	markets, err := s.fetchMarkets()
	if err != nil {
		return nil, err
	}
	log.Printf("[scanner] fetched %d active markets from Gamma API", len(markets))

	var opps []Opportunity
	for _, m := range markets {
		if !s.qualifies(m, alreadyTraded, activePositions) {
			continue
		}
		sport := DetectSport(m.Slug)
		side, price, err := bestOutcome(m)
		if err != nil {
			log.Printf("[scanner] skip %s: %v", m.ConditionID[:12], err)
			continue
		}
		if price < s.threshold {
			continue
		}

		rawSize := sizer(price)
		if rawSize <= 0 {
			continue
		}
		size := min64(rawSize, s.maxSize)
		shares := size / price

		opps = append(opps, Opportunity{
			ConditionID: m.ConditionID,
			Market:      m.Question,
			Slug:        m.Slug,
			Sport:       sport,
			Side:        side,
			Price:       price,
			Shares:      shares,
			SizeUSDC:    size,
			Icon:        m.Icon,
		})
	}
	return opps, nil
}

// qualifies runs the pre-price checks on a market.
func (s *Scanner) qualifies(m gammaMarket, traded, positions map[string]bool) bool {
	if !m.Active || m.Closed {
		return false
	}
	if traded[m.ConditionID] || positions[m.ConditionID] {
		return false
	}
	sport := DetectSport(m.Slug)
	if !s.sportAllowed(sport) {
		return false
	}
	// Check time to close if end date is present
	if m.EndDateISO != "" {
		t, err := time.Parse(time.RFC3339, m.EndDateISO)
		if err == nil && time.Until(t) < time.Duration(s.minHoursToClose*float64(time.Hour)) {
			return false
		}
	}
	return true
}

func (s *Scanner) sportAllowed(sport string) bool {
	for _, allowed := range s.sports {
		if allowed == sport {
			return true
		}
	}
	return false
}

// bestOutcome parses the outcomePrices/outcomes fields and returns the
// highest-priced outcome label and its price.
func bestOutcome(m gammaMarket) (string, float64, error) {
	if m.OutcomePrices == "" || m.Outcomes == "" {
		return "", 0, fmt.Errorf("missing price data")
	}

	var priceStrs []string
	if err := json.Unmarshal([]byte(m.OutcomePrices), &priceStrs); err != nil {
		return "", 0, fmt.Errorf("parse outcomePrices: %w", err)
	}
	var outcomes []string
	if err := json.Unmarshal([]byte(m.Outcomes), &outcomes); err != nil {
		return "", 0, fmt.Errorf("parse outcomes: %w", err)
	}
	if len(priceStrs) == 0 || len(priceStrs) != len(outcomes) {
		return "", 0, fmt.Errorf("mismatched price/outcome arrays")
	}

	bestLabel := ""
	bestPrice := 0.0
	for i, ps := range priceStrs {
		var p float64
		fmt.Sscanf(ps, "%f", &p)
		if p > bestPrice {
			bestPrice = p
			if i < len(outcomes) {
				bestLabel = outcomes[i]
			}
		}
	}
	if bestLabel == "" {
		return "", 0, fmt.Errorf("could not determine best outcome")
	}
	return bestLabel, bestPrice, nil
}

// fetchMarkets retrieves all active, unclosed markets from the Gamma API.
func (s *Scanner) fetchMarkets() ([]gammaMarket, error) {
	url := "https://gamma-api.polymarket.com/markets?active=true&closed=false&limit=500"
	resp, err := s.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("gamma API request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gamma API returned %d", resp.StatusCode)
	}

	var markets []gammaMarket
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return nil, fmt.Errorf("decode gamma response: %w", err)
	}
	return markets, nil
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
