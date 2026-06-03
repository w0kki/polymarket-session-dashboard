package market

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	"csl": "Soccer", "ksl": "Soccer", "jsl": "Soccer", "asl": "Soccer",
	"bra": "Soccer", "tur": "Soccer", "nor": "Soccer", "fif": "Soccer",
	"mlb": "Baseball", "kbo": "Baseball",
	"nba": "Basketball", "wnba": "Basketball",
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
	NegRisk         bool        `json:"neg_risk"`
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
	Market      string // human-readable title
	Slug        string
	Sport       string
	Side        string  // winning outcome label, e.g. "Arizona Diamondbacks"
	TokenID     string  // CLOB token_id — required for live order placement
	Price       float64 // token price (e.g. 0.94)
	Shares      float64 // position_size / price
	SizeUSDC    float64 // final position size after Kelly + cap
	MaxPrice    float64 // sport price ceiling — used to compute takerAmount slippage tolerance
	Icon        string
	NegRisk     bool // true = Neg Risk CTF Exchange; false = regular CTF Exchange
	PaperOnly   bool // execute via the paper executor only (never live) — e.g. doubles
}

// ── Scanner ───────────────────────────────────────────────────────────────────

// SportBounds overrides the global entry threshold and max price for a specific
// sport. All fields are optional: a zero value means "use the global default".
type SportBounds struct {
	MinPrice        float64 // 0 = use global EntryThreshold
	MaxPrice        float64 // 0 = use global MaxEntryPrice
	MaxHoursToClose float64 // 0 = no restriction; >0 = only trade when this many hours or less remain
	//                         Soccer example: 0.5 = only trade in final 30 min ≈ 70th minute onward
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
	paperDoubles    bool // if true, allow doubles into the watchlist tagged PaperOnly (paper-trade them)
	client          *http.Client

	gameIDMu    sync.Mutex     // guards gameIDCache
	gameIDCache map[string]int // slug → gameId (0 = resolved-but-none)
}

func NewScanner(threshold, maxPrice float64, sports []string, maxSize, minHoursToClose, maxHoursToClose, minVolume float64, sportBounds map[string]SportBounds, paperDoubles bool) *Scanner {
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
		paperDoubles:    paperDoubles,
		client:          &http.Client{Timeout: 20 * time.Second},
		gameIDCache:     map[string]int{},
	}
}

// isDoubles reports whether a market is a tennis doubles match (two-player teams).
func isDoubles(m clobMarket) bool {
	return strings.Contains(strings.ToLower(m.MarketSlug), "doubles") ||
		strings.Contains(strings.ToLower(m.Question), "(doubles)")
}

// ResolveGameID maps a market slug to its Polymarket sports gameId via the
// Gamma events API. Cached in memory (slug→gameId) since the mapping is static.
// Returns (0, false) when the event has no associated gameId (non-sports market).
func (s *Scanner) ResolveGameID(slug string) (int, bool) {
	s.gameIDMu.Lock()
	if id, ok := s.gameIDCache[slug]; ok {
		s.gameIDMu.Unlock()
		return id, id != 0
	}
	s.gameIDMu.Unlock()

	url := fmt.Sprintf("https://gamma-api.polymarket.com/events?slug=%s", slug)
	resp, err := s.client.Get(url)
	if err != nil {
		return 0, false // transient — don't cache failures
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, false
	}
	var events []struct {
		GameID int `json:"gameId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil || len(events) == 0 {
		return 0, false
	}
	id := events[0].GameID

	s.gameIDMu.Lock()
	s.gameIDCache[slug] = id
	s.gameIDMu.Unlock()
	return id, id != 0
}

// periodNum parses a tennis period string ("S3", "TB4") into its set number.
// Returns 0 if unparseable.
func periodNum(period string) int {
	p := strings.ToUpper(strings.TrimSpace(period))
	p = strings.TrimPrefix(p, "TB")
	p = strings.TrimPrefix(p, "S")
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0
	}
	return n
}

// currentSetMaxGames parses the games count of the current set from a tennis
// score string and returns the higher of the two players' game counts.
// Score formats: "2-1" (current set only) or "6-4, 3-6, 5-4" (set history,
// last segment = current set). Returns 0 if unparseable.
func currentSetMaxGames(score string) int {
	score = strings.TrimSpace(score)
	if score == "" {
		return 0
	}
	segs := strings.Split(score, ",")
	last := strings.TrimSpace(segs[len(segs)-1])
	// Strip any tiebreak annotation: "7-6(7-5)" → "7-6".
	if i := strings.Index(last, "("); i >= 0 {
		last = last[:i]
	}
	parts := strings.SplitN(last, "-", 2)
	if len(parts) != 2 {
		return 0
	}
	a, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	b, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	if a > b {
		return a
	}
	return b
}

// TennisSetStageOK reports whether a tennis match is far enough along to trade,
// given minSet (e.g. 3). Allows entry when:
//   - the match is in set minSet or later (e.g. "during the 3rd set"), OR
//   - it is in the immediately prior set with someone serving for it
//     (≥5 games — i.e. "the end of the 2nd set").
func TennisSetStageOK(period, score string, minSet int) bool {
	if minSet <= 0 {
		return true // gating disabled
	}
	n := periodNum(period)
	if n == 0 {
		return false // unknown set — fail closed
	}
	if n >= minSet {
		return true
	}
	if n == minSet-1 && currentSetMaxGames(score) >= 5 {
		return true
	}
	return false
}

// periodInt parses the leading integer from a game period string — the inning
// for baseball ("Top 6th" → 6) or the period for hockey ("End P1" → 1, "P2" → 2).
// Returns 0 if no number is found.
func periodInt(period string) int {
	n := 0
	found := false
	for _, r := range period {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
			found = true
		} else if found {
			break // digits are contiguous; stop at the first non-digit after them
		}
	}
	if !found {
		return 0
	}
	return n
}

// runDiff parses the absolute run differential from a baseball score string
// such as "8-3" → 5. Returns 0 if unparseable.
func runDiff(score string) int {
	parts := strings.SplitN(strings.TrimSpace(score), "-", 2)
	if len(parts) != 2 {
		return 0
	}
	a, errA := strconv.Atoi(strings.TrimSpace(parts[0]))
	b, errB := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errA != nil || errB != nil {
		return 0
	}
	if a > b {
		return a - b
	}
	return b - a
}

// GameStageOK reports whether a period-based game (baseball innings, hockey
// periods) is far enough along to trade. Allows entry when the game has reached
// minPeriod OR the score differential is at least minDiff (a blowout, decided
// early). Either threshold being 0 disables that arm of the check.
func GameStageOK(period, score string, minPeriod, minDiff int) bool {
	if minPeriod <= 0 && minDiff <= 0 {
		return true // gating disabled
	}
	p := periodInt(period)
	diff := runDiff(score)
	if p == 0 && diff == 0 {
		return false // no usable live state — fail closed
	}
	if minPeriod > 0 && p >= minPeriod {
		return true
	}
	if minDiff > 0 && diff >= minDiff {
		return true
	}
	return false
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
	NegRisk     bool // true = Neg Risk CTF Exchange; false = regular CTF Exchange
	PaperOnly   bool // route to the paper executor regardless of live mode (e.g. doubles)
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
			NegRisk:     m.NegRisk,
			PaperOnly:   isDoubles(m), // doubles only reach here when paperDoubles is on
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

	// Per-sport time window — e.g. Soccer only trades in final 30 min (≈70th minute onward).
	// Uses EndDateISO as a proxy: if more time remains than the sport's MaxHoursToClose, skip.
	if bounds, ok := s.sportBounds[entry.Sport]; ok && bounds.MaxHoursToClose > 0 {
		if m.EndDateISO != "" {
			endTime, err := time.Parse(time.RFC3339, m.EndDateISO)
			if err != nil {
				t2, err2 := time.Parse("2006-01-02", m.EndDateISO)
				if err2 == nil {
					endTime = t2.Add(24 * time.Hour)
					err = nil
				}
			}
			if err == nil && time.Until(endTime).Hours() > bounds.MaxHoursToClose {
				return nil, nil // not yet in the trading window for this sport
			}
		}
	}

	side, stalePrice := bestOutcome(*m)
	if side == "" {
		return nil, nil
	}

	// Look up the token_id for the favored side up front — needed both for the
	// live order-book lookup and for live order placement.
	tokenID := ""
	for _, tok := range m.Tokens {
		if tok.Outcome == side {
			tokenID = tok.TokenID
			break
		}
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

	// token.price is the last-trade price and lags the live book in fast
	// markets — sometimes by 15¢+ (e.g. it read 82.5¢ for a favorite whose live
	// ask was 98¢, and 90.5¢ for one whose live ask was 96¢). When the stale
	// price is within a generous window of the band, confirm against the live
	// order book and use the best ASK — the price we'd actually pay — for the
	// entry decision. The 20¢ window covers the observed staleness while keeping
	// book fetches limited to plausible-favorite markets.
	price := stalePrice
	if tokenID != "" && stalePrice >= minPrice-0.20 {
		if _, ask, ok := s.fetchBook(tokenID); ok {
			if (ask >= minPrice && (maxPrice <= 0 || ask <= maxPrice)) && ask != stalePrice {
				log.Printf("[scanner] %s: live ask %.3f (token.price %.3f stale) — using book", side, ask, stalePrice)
			}
			price = ask
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
			// Fail CLOSED: if we can't verify volume we skip rather than risk a
			// low-volume entry. The opportunity is re-checked on the next poll.
			log.Printf("[scanner] volume check error for %s: %v — skipping (fail-closed)", entry.ConditionID[:12], err)
			return nil, nil
		}
		if vol < s.minVolume {
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
		TokenID:     tokenID,
		Price:       price,
		Shares:      shares,
		SizeUSDC:    size,
		Icon:        m.Icon,
		NegRisk:     entry.NegRisk,
		MaxPrice:    maxPrice,
		PaperOnly:   entry.PaperOnly,
	}, nil
}

// GetNegRisk queries the CLOB to determine which CTF Exchange contract a token
// belongs to. Returns true for the Neg Risk exchange, false for the regular one.
// Called at stop-loss time (rare) so the extra HTTP request is acceptable.
func (s *Scanner) GetNegRisk(tokenID string) (bool, error) {
	url := fmt.Sprintf("https://clob.polymarket.com/neg-risk?token_id=%s", tokenID)
	resp, err := s.client.Get(url)
	if err != nil {
		return false, fmt.Errorf("neg-risk API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("neg-risk API returned %d", resp.StatusCode)
	}
	var result struct {
		NegRisk bool `json:"neg_risk"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("decode neg-risk response: %w", err)
	}
	return result.NegRisk, nil
}

// fetchVolume calls the Gamma API to get the total trading volume (USD) for a
// market. Only called when a market's price has already qualified, so the
// extra HTTP round-trip only occurs on genuine trade candidates.
func (s *Scanner) fetchVolume(conditionID string) (float64, error) {
	// IMPORTANT: the filter param is condition_ids (snake_case, plural). The
	// camelCase conditionId is silently IGNORED by gamma — it returns a default
	// page of ~20 unrelated markets, so markets[0].volumeNum was the volume of
	// some random high-volume market (~$778K), making the volume floor pass for
	// every market. condition_ids returns exactly the one matching market.
	url := fmt.Sprintf("https://gamma-api.polymarket.com/markets?condition_ids=%s", conditionID)
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
		return 0, nil // market not found → treat as zero volume (skips on the floor)
	}
	// Guard: the correct param returns exactly one market. If we somehow get a
	// page back, the filter wasn't honored — don't trust markets[0].
	if len(markets) > 1 {
		return 0, fmt.Errorf("gamma returned %d markets for one conditionId — filter not honored", len(markets))
	}
	return markets[0].VolumeNum, nil
}

// fetchBook returns the best bid and best ask for a token from the live CLOB
// order book. Used to get an up-to-date entry price, since the market
// endpoint's token.price field is the last-trade price and lags the book in
// fast-moving live markets. Returns ok=false on error or an empty ask side.
func (s *Scanner) fetchBook(tokenID string) (bestBid, bestAsk float64, ok bool) {
	url := fmt.Sprintf("https://clob.polymarket.com/book?token_id=%s", tokenID)
	resp, err := s.client.Get(url)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, false
	}
	var book struct {
		Bids []struct {
			Price string `json:"price"`
		} `json:"bids"`
		Asks []struct {
			Price string `json:"price"`
		} `json:"asks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&book); err != nil {
		return 0, 0, false
	}
	for _, b := range book.Bids {
		if p, err := strconv.ParseFloat(b.Price, 64); err == nil && p > bestBid {
			bestBid = p
		}
	}
	for _, a := range book.Asks {
		if p, err := strconv.ParseFloat(a.Price, 64); err == nil && (bestAsk == 0 || p < bestAsk) {
			bestAsk = p
		}
	}
	if bestAsk == 0 {
		return 0, 0, false
	}
	return bestBid, bestAsk, true
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
	// Doubles tennis: excluded by default (thin liquidity, noisy two-player
	// upset risk). When paperDoubles is enabled they're allowed THROUGH here so
	// they can be PAPER-traded — BuildWatchlist tags them PaperOnly and the poll
	// loop routes them to the paper executor, so they NEVER touch live capital.
	if isDoubles(m) && !s.paperDoubles {
		return false
	}
	// Must be a head-to-head game/match — filters novelty props and futures.
	// "vs." (with period, common in CLOB) and "vs " both pass.
	if !strings.Contains(strings.ToLower(m.Question), " vs") {
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

// GetSidePrice fetches a market and returns the current price for a specific
// outcome label (the side the bot is holding). Returns marketOpen=false when
// the market has settled or is no longer accepting orders — the caller should
// let the normal resolve loop handle those.
func (s *Scanner) GetSidePrice(conditionID, side string) (price float64, marketOpen bool, err error) {
	m, err := s.CheckMarket(conditionID)
	if err != nil {
		return 0, false, err
	}
	if !m.Active || m.Closed || !m.AcceptingOrders {
		return 0, false, nil // settled — let resolveOpenTrades handle it
	}
	for _, tok := range m.Tokens {
		if tok.Outcome == side {
			return tok.Price, true, nil
		}
	}
	return 0, true, nil // market open but side not found
}

// GetSidePriceAndToken fetches a market and returns the current price AND the
// CLOB token_id for the given outcome. Used by the live stop-loss handler to
// build a SELL order without needing to store the token_id at trade time.
func (s *Scanner) GetSidePriceAndToken(conditionID, side string) (price float64, tokenID string, marketOpen bool, err error) {
	m, err := s.CheckMarket(conditionID)
	if err != nil {
		return 0, "", false, err
	}
	if !m.Active || m.Closed || !m.AcceptingOrders {
		return 0, "", false, nil
	}
	for _, tok := range m.Tokens {
		if tok.Outcome == side {
			return tok.Price, tok.TokenID, true, nil
		}
	}
	return 0, "", true, nil
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
