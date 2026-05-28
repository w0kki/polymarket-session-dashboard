package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Notifier fans alerts out to all configured channels (Discord, Telegram, …).
// Any channel whose credentials are empty is silently skipped, so the bot
// works without any notifications configured.
type Notifier struct {
	discord  *discordChannel
	telegram *telegramChannel
	client   *http.Client
}

// New constructs a Notifier. Pass empty strings to disable individual channels.
func New(discordWebhookURL, telegramToken, telegramChatID string) *Notifier {
	client := &http.Client{Timeout: 10 * time.Second}
	n := &Notifier{client: client}
	if discordWebhookURL != "" {
		n.discord = &discordChannel{url: discordWebhookURL, client: client}
	}
	if telegramToken != "" && telegramChatID != "" {
		n.telegram = &telegramChannel{token: telegramToken, chatID: telegramChatID, client: client}
	}
	return n
}

// Enabled reports whether at least one notification channel is active.
func (n *Notifier) Enabled() bool {
	return n.discord != nil || n.telegram != nil
}

// send broadcasts a message to all active channels.
func (n *Notifier) send(msg string) {
	if n.discord != nil {
		n.discord.send(msg)
	}
	if n.telegram != nil {
		n.telegram.send(msg)
	}
}

// ── Typed alert methods ───────────────────────────────────────────────────────

// TradePlaced fires when a new paper (or live) order is submitted.
func (n *Notifier) TradePlaced(market, side, sport string, price, size float64) {
	n.send(fmt.Sprintf(
		"📝 TRADE PLACED (%s)\n%s\n▶ %s @ %.1f¢ | Size: $%.2f",
		sport, market, side, price*100, size,
	))
}

// TradeResolved fires when a paper trade settles.
// Silent for small wins (< $5) to reduce noise.
func (n *Notifier) TradeResolved(market, side, outcome string, pnl float64) {
	switch {
	case outcome == "LOSS":
		n.send(fmt.Sprintf(
			"📉 LOSS — %s\n%s\nP&L: -$%.2f",
			side, market, -pnl,
		))
	case outcome == "WIN" && pnl >= 5.0:
		n.send(fmt.Sprintf(
			"✅ WIN — %s\n%s\nP&L: +$%.2f",
			side, market, pnl,
		))
	}
}

// CircuitBreaker fires when N consecutive losses trigger a trading pause.
func (n *Notifier) CircuitBreaker(consecLosses int, until time.Time) {
	n.send(fmt.Sprintf(
		"⏸ CIRCUIT BREAKER TRIPPED\n%d consecutive losses — trading paused until %s UTC",
		consecLosses, until.UTC().Format("2006-01-02 15:04"),
	))
}

// CircuitBreakerCleared fires when an active breaker expires.
func (n *Notifier) CircuitBreakerCleared() {
	n.send("✅ Circuit breaker expired — trading resumed.")
}

// DailyLossLimit fires when today's P&L crosses the configured limit.
func (n *Notifier) DailyLossLimit(todayPnL, limit float64) {
	n.send(fmt.Sprintf(
		"🚨 DAILY LOSS LIMIT HIT\nToday P&L: -$%.2f | Limit: $%.2f\nNo new trades until midnight UTC.",
		-todayPnL, limit,
	))
}

// BankrollFloor fires just before the bot shuts itself down.
func (n *Notifier) BankrollFloor(balance, floor float64) {
	n.send(fmt.Sprintf(
		"🚨 BANKROLL FLOOR BREACHED — BOT SHUTTING DOWN\nBalance: $%.2f | Floor: $%.2f\nManual restart required.",
		balance, floor,
	))
}

// ── Discord ───────────────────────────────────────────────────────────────────

type discordChannel struct {
	url    string
	client *http.Client
}

func (d *discordChannel) send(content string) {
	payload := map[string]string{"content": content}
	body, _ := json.Marshal(payload)
	resp, err := d.client.Post(d.url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[notify/discord] send error: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[notify/discord] status %d", resp.StatusCode)
	}
}

// ── Telegram ──────────────────────────────────────────────────────────────────

type telegramChannel struct {
	token  string
	chatID string
	client *http.Client
}

func (t *telegramChannel) send(text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
	payload := map[string]string{
		"chat_id": t.chatID,
		"text":    text,
	}
	body, _ := json.Marshal(payload)
	resp, err := t.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[notify/telegram] send error: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[notify/telegram] status %d", resp.StatusCode)
	}
}
