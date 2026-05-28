package market

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// moneylineSlug matches slugs that end in a bare date: sport-p1-p2-YYYY-MM-DD
// Props and O/U markets have extra suffixes (-match-total-21pt5, etc.).
var moneylineSlug = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

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

// ── CLOB API types ────────────────────────────────────────────────────────────

type clobToken struct {
	TokenID string  `json:"token_id"`
	Outcome string  `json:"outcome"`
	Price   float64 `json:"price"`
	Winner  bool    `json:"winner"`
}

type clobMarket struct {
	ConditionID     string      `json:"condition_id"`
	Question        string      `json:"question"`
	MarketSlug      string      `json:"market_slug"`
	Active          bool        `json:"active"`
	Closed          bool        `json:"closed"`
	AcceptingOrders bool        `json:"accepting_orders"`
	EndDateISO      string      `json:"end_date_iso"`
	Tokens          []clobToken `json:"tokens"`
	Icon            string      `json:"icon"`
}

type clobResponse struct {
	Data       []clobMarket `json:"data"`
	NextCursor string       `json:"next_cursor"`
	Count      int          `json:"count"`
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

// SportBounds overrides the global entry threshold and max price for a specific
// sport. Both fields are optional: a zero value means "use the global default".
type SportBounds struct {
	MinPrice float64 // 0 = use global EntryThreshold
	MaxPrice float64 // 0 = use global MaxEntryPrice
}

type Scanner struct {
	threshold       float64
	maxPrice        float64
	sports          []string
	maxSize         float64
	minHoursToClose float64
	maxHoursToClose float64
	minVolume       float64 // minimum total market volume in USD (0 = disabled)
	sportBounds     map[string]SportBounds
	client          *http.Client
}

func NewScanner(threshold, maxPrice float64, sports []string, maxSize, minHoursToClose, maxHoursToClose, minVolume float64, sportBounds map[string]SportBounds) *Scanner {
	if sportBounds == nil {
		sportBounds = map[string]SportBounds{}
	}
	return &Scanner{
		threshold:       threshold,
		maxPrice:        maxPrice,
		sports:          sports,
		maxSize:         maxSize,
		minHoursToClose: minHoursToClose,
		maxHoursToClose: maxHoursToClose,
		minVolume:       minVolume,
		sportBounds:     sportBounds,
		client:          &http.Client{Timeout: 20 * time.Second},
	}
}

// WatchlistEntry is a market that passed all non-price filters and is being
// actively monitored for a qualifying price. Built by BuildWatchlist (slow
// loop) and polled by PollOpportunity (fast loop).
type WatchlistEntry struct {
	ConditionID string
	Market      string // human-readable title
	Slug        string
	Sport       string
	Icon        string
}

// BuildWatchlist does a full market scan and returns every market that passes
// all structural filters (sport, moneyline, doubles, hours) but WITHOUT price
// filtering. Called infrequently (~every 10 min) to refresh the watchlist.
func (s *Scanner) BuildWatchlist(alreadyTraded, activePositions map[string]bool) ([]WatchlistEntry, error) {
	markets, err := s.fetchMarkets()
	if err != nil {
		return nil, err
	}
	log.Printf("[scanner] discovery: fetched %d markets", len(markets))

	var entries []WatchlistEntry
	for _, m := range markets {
		if !s.qualifies(m, alreadyTraded, activePositions) {
			continue
		}
		sport := DetectSport(m.MarketSlug)
		entries = append(entries, WatchlistEntry{
			ConditionID: m.ConditionID,
			Market:      m.Question,
			Slug:        m.MarketSlug,
			Sport:       sport,
			Icon:        m.Icon,
		})
	}
	return entries, nil
}

// PollOpportunity fetches a single market by condition ID and returns an
// Opportunity if the current price falls within the sport-specific bounds.
// Returns nil (no error) when the price doesn't qualify or the market is closed.
func (s *Scanner) PollOpportunity(entry WatchlistEntry, sizer func(float64) float64) (*Opportunity, error) {
	m, err := s.CheckMarket(entry.ConditionID)
	if err != nil {
		return nil, err
	}
	if !m.Active || m.Closed || !m.AcceptingOrders {
		return nil, nil // market has expired or closed since last discovery
	}

	side, price := bestOutcome(*m)
	if side == "" {
		return nil, nil
	}

	// Apply per-sport price bounds (same logic as Scan)
	minPrice := s.threshold
	maxPrice := s.maxPrice
	if bounds, ok := s.sportBounds[entry.Sport]; ok {
		if bounds.MinPrice > 0 {
			minPrice = bounds.MinPrice
		}
		if bounds.MaxPrice > 0 {
			maxPrice = bounds.MaxPrice
		}
	}
	if price < minPrice || (maxPrice > 0 && price > maxPrice) {
		return nil, nil
	}

	// Volume check — only call Gamma API when price already qualifies to keep
	// the hot path (no-op polls) free of extra HTTP requests.
	if s.minVolume > 0 {
		vol, err := s.fetchVolume(entry.ConditionID)
		if err != nil {
			log.Printf("[scanner] volume check error for %s: %v — allowing trade", entry.ConditionID[:12], err)
		} else if vol < s.minVolume {
			log.Printf("[scanner] skip %s — volume $%.0f below $%.0f threshold", entry.ConditionID[:12], vol, s.minVolume)
			return nil, nil
		}
	}

	rawSize := sizer(price)
	if rawSize <= 0 {
		return nil, nil
	}
	size := min64(rawSize, s.maxSize)
	shares := size / price

	return &Opportunity{
		ConditionID: m.ConditionID,
		Market:      m.Question,
		Slug:        m.MarketSlug,
		Sport:       entry.Sport,
		Side:        side,
		Price:       price,
		Shares:      shares,
		SizeUSDC:    size,
		Icon:        m.Icon,
	}, nil
}

// fetchVolume calls the Gamma API to get the total trading volume (USD) for a
// market. Only called when a market's price has already qualified, so the
// extra HTTP round-trip only occurs on genuine trade candidates.
func (s *Scanner) fetchVolume(conditionID string) (float64, error) {
	url := fmt.Sprintf("https://gamma-api.polymarket.com/markets?conditionId=%s", conditionID)
	resp, err := s.client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("gamma API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("gamma API returned %d", resp.StatusCode)
	}

	var markets []struct {
		VolumeNum float64 `json:"volumeNum"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return 0, fmt.Errorf("decode gamma response: %w", err)
	}
	if len(markets) == 0 {
		return 0, nil
	}
	return markets[0].VolumeNum, nil
}

// Scan fetches active markets and returns those that pass all filters.
//
//	alreadyTraded   – conditionIds already in the trades table (skip)
//	activePositions – conditionIds currently held in the wallet (skip)
//	sizer           – function that returns the desired $ size for a price
func (s *Scanner) Scan(
	alreadyTraded map[string]bool,
	activePositions map[string]bool,
	sizer func(price float64) float64,
) ([]Opportunity, error) {
	markets, err := s.fetchMarkets()
	if err != nil {
		return nil, err
	}
	log.Printf("[scanner] fetched %d markets from CLOB API", len(markets))

	var opps []Opportunity
	for _, m := range markets {
		if !s.qualifies(m, alreadyTraded, activePositions) {
			continue
		}
		sport := DetectSport(m.MarketSlug)
		side, price := bestOutcome(m)
		if side == "" {
			continue
		}

		// Apply per-sport price bounds if set; otherwise use global thresholds.
		minPrice := s.threshold
		maxPrice := s.maxPrice
		if bounds, ok := s.sportBounds[sport]; ok {
			if bounds.MinPrice > 0 {
				minPrice = bounds.MinPrice
			}
			if bounds.MaxPrice > 0 {
				maxPrice = bounds.MaxPrice
			}
		}

		if price < minPrice {
			continue
		}
		if maxPrice > 0 && price > maxPrice {
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
			Slug:        m.MarketSlug,
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
func (s *Scanner) qualifies(m clobMarket, traded, positions map[string]bool) bool {
	if !m.Active || m.Closed || !m.AcceptingOrders {
		return false
	}
	if traded[m.ConditionID] || positions[m.ConditionID] {
		return false
	}
	sport := DetectSport(m.MarketSlug)
	if !s.sportAllowed(sport) {
		return false
	}
	// Only trade the moneyline (match-winner) market.
	// Props, O/U, and set markets append suffixes after the date in the slug.
	if !moneylineSlug.MatchString(m.MarketSlug) {
		return false
	}
	// Exclude doubles matches — slug contains "doubles" prefix segment.
	if strings.Contains(strings.ToLower(m.MarketSlug), "doubles") {
		return false
	}
	// Must be a head-to-head game/match — filters novelty props and futures.
	// "vs." (with period, common in CLOB) and "vs " both pass.
	if !strings.Contains(strings.ToLower(m.Question), " vs") {
		return false
	}
	// Skip doubles tennis markets — the per-share upside at 96–99¢ is too
	// small to justify the upset risk in a two-player team format.
	// Check both the question text AND the slug (slugs always contain "-doubles-")
	// so neither variation can slip through.
	if strings.Contains(strings.ToLower(m.Question), "(doubles)") ||
		strings.Contains(strings.ToLower(m.MarketSlug), "-doubles-") {
		return false
	}
	// Skip Hamburg European Open — ATP 500 clay event where top-50 players
	// are tightly matched pre-Roland Garros; empirical loss rate is 31%,
	// far above market-implied probability at the 94–96¢ range.
	if strings.Contains(strings.ToLower(m.MarketSlug), "hamburg") {
		return false
	}
	// End date is required — skip if missing or unparseable
	if m.EndDateISO == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, m.EndDateISO)
	if err != nil {
		// Fall back to date-only format e.g. "2026-05-19".
		// Add 24h so the full calendar day is valid regardless of Pi timezone.
		t, err = time.Parse("2006-01-02", m.EndDateISO)
		if err != nil {
			return false
		}
		t = t.Add(24 * time.Hour)
	}
	hoursToClose := time.Until(t).Hours()
	if hoursToClose < s.minHoursToClose {
		return false
	}
	if s.maxHoursToClose > 0 && hoursToClose > s.maxHoursToClose {
		return false
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

// bestOutcome returns the highest-priced outcome label and its price from the
// market's token list. Returns ("", 0) if no tokens are present.
func bestOutcome(m clobMarket) (string, float64) {
	bestLabel := ""
	bestPrice := 0.0
	for _, t := range m.Tokens {
		if t.Price > bestPrice {
			bestPrice = t.Price
			bestLabel = t.Outcome
		}
	}
	return bestLabel, bestPrice
}

// fetchMarkets paginates through all markets from the CLOB API using
// cursor-based pagination. Returns only active, non-closed markets.
//
// The CLOB API sorts oldest-first. Live 2026 markets are at offsets ~650k+,
// so we skip the historical backlog by starting well into the pagination.
// "NjUwMDAw" = base64("650000"); adjust upward as the market count grows.
func (s *Scanner) fetchMarkets() ([]clobMarket, error) {
	const maxPages = 200 // safety cap — plenty of room beyond the current ~84 pages remaining
	var all []clobMarket
	// CLOB API has ~1,184 pages total sorted oldest-first.
	// May 2026 markets begin at page ~1,050; today's live matches at ~1,170.
	// Start at offset 1,100,000 (page 1,100) to scan only the recent tail.
	// "MTEwMDAwMA==" = base64("1100000")
	cursor := "MTEwMDAwMA=="

	for page := 0; page < maxPages; page++ {
		url := fmt.Sprintf("https://clob.polymarket.com/markets?next_cursor=%s", cursor)
		resp, err := s.client.Get(url)
		if err != nil {
			return nil, fmt.Errorf("CLOB API request (page=%d): %w", page, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("CLOB API returned %d (page=%d)", resp.StatusCode, page)
		}

		var page_resp clobResponse
		if err := json.NewDecoder(resp.Body).Decode(&page_resp); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decode CLOB response (page=%d): %w", page, err)
		}
		resp.Body.Close()

		all = append(all, page_resp.Data...)

		if page_resp.NextCursor == "" || page_resp.NextCursor == cursor || page_resp.Count < 1000 {
			break
		}
		cursor = page_resp.NextCursor
	}
	return all, nil
}

// CheckMarket fetches a single market by condition ID from the CLOB API.
// Used by the resolver to check if an open paper trade has settled.
func (s *Scanner) CheckMarket(conditionID string) (*clobMarket, error) {
	url := fmt.Sprintf("https://clob.polymarket.com/markets/%s", conditionID)
	resp, err := s.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch market %s: %w", conditionID[:12], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("market %s returned %d", conditionID[:12], resp.StatusCode)
	}
	var m clobMarket
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode market %s: %w", conditionID[:12], err)
	}
	return &m, nil
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
