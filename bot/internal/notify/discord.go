package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Notifier sends messages to a Discord channel via webhook.
// If the webhook URL is empty all methods are silent no-ops so the
// bot works identically when notifications are not configured.
type Notifier struct {
	webhookURL string
	client     *http.Client
}

// New returns a Notifier. Pass an empty string to disable notifications.
func New(webhookURL string) *Notifier {
	return &Notifier{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether a webhook URL has been configured.
func (n *Notifier) Enabled() bool { return n.webhookURL != "" }

// send is the low-level Discord webhook call.
func (n *Notifier) send(content string) {
	if n.webhookURL == "" {
		return
	}
	payload := map[string]string{"content": content}
	body, _ := json.Marshal(payload)
	resp, err := n.client.Post(n.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[notify] discord send error: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[notify] discord returned %d", resp.StatusCode)
	}
}

// ── Typed alert methods ───────────────────────────────────────────────────────

// CircuitBreaker fires when N consecutive losses trigger a trading pause.
func (n *Notifier) CircuitBreaker(consecLosses int, until time.Time) {
	n.send(fmt.Sprintf(
		"⏸ **CIRCUIT BREAKER TRIPPED**\n%d consecutive losses — trading paused until **%s UTC**",
		consecLosses, until.UTC().Format("2006-01-02 15:04"),
	))
}

// BankrollFloor fires just before the bot shuts itself down.
func (n *Notifier) BankrollFloor(balance, floor float64) {
	n.send(fmt.Sprintf(
		"🚨 **BANKROLL FLOOR BREACHED — BOT SHUTTING DOWN**\nBalance: **$%.2f** | Floor: $%.2f\nManual restart required after investigation.",
		balance, floor,
	))
}

// DailyLossLimit fires when today's P&L crosses the configured limit.
func (n *Notifier) DailyLossLimit(todayPnL, limit float64) {
	n.send(fmt.Sprintf(
		"🚨 **DAILY LOSS LIMIT HIT**\nToday P&L: **-$%.2f** | Limit: $%.2f\nNo new trades until midnight UTC.",
		-todayPnL, limit,
	))
}

// TradePlaced fires when a new paper (or live) order is submitted.
func (n *Notifier) TradePlaced(market, side, sport string, price, size float64) {
	n.send(fmt.Sprintf(
		"📝 **TRADE PLACED** (%s)\n%s\n▶ **%s** @ %.1f¢ | Size: $%.2f",
		sport, market, side, price*100, size,
	))
}

// TradeResolved fires when a paper trade settles.
// Only sends for losses or significant wins (> $5) to avoid noise.
func (n *Notifier) TradeResolved(market, side, outcome string, pnl float64) {
	switch {
	case outcome == "LOSS":
		n.send(fmt.Sprintf(
			"📉 **LOSS** — %s\n%s\nP&L: **-$%.2f**",
			side, market, -pnl,
		))
	case outcome == "WIN" && pnl >= 5.0:
		n.send(fmt.Sprintf(
			"✅ **WIN** — %s\n%s\nP&L: **+$%.2f**",
			side, market, pnl,
		))
	}
}

// CircuitBreakerCleared fires when an active breaker expires.
func (n *Notifier) CircuitBreakerCleared() {
	n.send("✅ **Circuit breaker expired** — trading resumed.")
}
