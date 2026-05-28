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

// broadcast sends plain text to all channels (used for system alerts without a link).
func (n *Notifier) broadcast(plain string) {
	if n.discord != nil {
		n.discord.send(plain)
	}
	if n.telegram != nil {
		n.telegram.sendPlain(plain)
	}
}

// broadcastLink sends a message that contains a hyperlink.
// Discord receives Markdown  [title](url);  Telegram receives HTML <a href>.
func (n *Notifier) broadcastLink(discordMsg, telegramHTML string) {
	if n.discord != nil {
		n.discord.send(discordMsg)
	}
	if n.telegram != nil {
		n.telegram.sendHTML(telegramHTML)
	}
}

// polyURL builds the Polymarket event page URL from a slug.
func polyURL(slug string) string {
	if slug == "" {
		return ""
	}
	return "https://polymarket.com/event/" + slug
}

// ── Typed alert methods ───────────────────────────────────────────────────────

// TradePlaced fires when a new order is submitted.
// The market title is rendered as a clickable link to the Polymarket event page.
func (n *Notifier) TradePlaced(market, side, sport, slug string, price, size float64) {
	url := polyURL(slug)
	discord := fmt.Sprintf(
		"📝 **TRADE PLACED** — Moneyline · %s\n[%s](%s)\n▶ %s @ %.1f¢ | Size: $%.2f",
		sport, market, url, side, price*100, size,
	)
	telegram := fmt.Sprintf(
		"📝 <b>TRADE PLACED</b> — Moneyline · %s\n<a href=\"%s\">%s</a>\n▶ %s @ %.1f¢ | Size: $%.2f",
		sport, url, market, side, price*100, size,
	)
	if url == "" {
		// No slug — fall back to plain text.
		n.broadcast(fmt.Sprintf("📝 TRADE PLACED — Moneyline · %s\n%s\n▶ %s @ %.1f¢ | Size: $%.2f",
			sport, market, side, price*100, size))
		return
	}
	n.broadcastLink(discord, telegram)
}

// TradeResolved fires when a trade settles as WIN or LOSS.
// Silent for small wins (< $5) to reduce noise.
func (n *Notifier) TradeResolved(market, side, outcome, slug string, pnl float64) {
	url := polyURL(slug)
	switch {
	case outcome == "LOSS":
		discord := fmt.Sprintf("📉 **LOSS** — Moneyline\n[%s](%s)\n▶ %s | P&L: -$%.2f", market, url, side, -pnl)
		telegram := fmt.Sprintf("📉 <b>LOSS</b> — Moneyline\n<a href=\"%s\">%s</a>\n▶ %s | P&L: -$%.2f", url, market, side, -pnl)
		if url == "" {
			n.broadcast(fmt.Sprintf("📉 LOSS — %s\n%s\nP&L: -$%.2f", side, market, -pnl))
			return
		}
		n.broadcastLink(discord, telegram)
	case outcome == "WIN" && pnl >= 5.0:
		discord := fmt.Sprintf("✅ **WIN** — Moneyline\n[%s](%s)\n▶ %s | P&L: +$%.2f", market, url, side, pnl)
		telegram := fmt.Sprintf("✅ <b>WIN</b> — Moneyline\n<a href=\"%s\">%s</a>\n▶ %s | P&L: +$%.2f", url, market, side, pnl)
		if url == "" {
			n.broadcast(fmt.Sprintf("✅ WIN — %s\n%s\nP&L: +$%.2f", side, market, pnl))
			return
		}
		n.broadcastLink(discord, telegram)
	}
}

// StopLossTriggered fires when a position is exited early via stop loss.
func (n *Notifier) StopLossTriggered(market, side, sport, slug string, exitPrice, netPnl, saved float64) {
	url := polyURL(slug)
	discord := fmt.Sprintf(
		"⛔ **STOP LOSS** — Moneyline · %s\n[%s](%s)\n▶ %s exited @ %.1f¢ | Loss: -$%.2f | Saved $%.2f vs full loss",
		sport, market, url, side, exitPrice*100, -netPnl, saved,
	)
	telegram := fmt.Sprintf(
		"⛔ <b>STOP LOSS</b> — Moneyline · %s\n<a href=\"%s\">%s</a>\n▶ %s exited @ %.1f¢ | Loss: -$%.2f | Saved $%.2f vs full loss",
		sport, url, market, side, exitPrice*100, -netPnl, saved,
	)
	if url == "" {
		n.broadcast(fmt.Sprintf("⛔ STOP LOSS (%s)\n%s\n▶ %s exited @ %.1f¢ | Loss: -$%.2f | Saved $%.2f",
			sport, market, side, exitPrice*100, -netPnl, saved))
		return
	}
	n.broadcastLink(discord, telegram)
}

// CircuitBreaker fires when N consecutive losses trigger a trading pause.
func (n *Notifier) CircuitBreaker(consecLosses int, until time.Time) {
	n.broadcast(fmt.Sprintf(
		"⏸ CIRCUIT BREAKER TRIPPED\n%d consecutive losses — trading paused until %s UTC",
		consecLosses, until.UTC().Format("2006-01-02 15:04"),
	))
}

// CircuitBreakerCleared fires when an active breaker expires.
func (n *Notifier) CircuitBreakerCleared() {
	n.broadcast("✅ Circuit breaker expired — trading resumed.")
}

// DailyLossLimit fires when today's P&L crosses the configured limit.
func (n *Notifier) DailyLossLimit(todayPnL, limit float64) {
	n.broadcast(fmt.Sprintf(
		"🚨 DAILY LOSS LIMIT HIT\nToday P&L: -$%.2f | Limit: $%.2f\nNo new trades until midnight UTC.",
		-todayPnL, limit,
	))
}

// BankrollFloor fires just before the bot shuts itself down.
func (n *Notifier) BankrollFloor(balance, floor float64) {
	n.broadcast(fmt.Sprintf(
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

func (t *telegramChannel) sendPlain(text string) {
	t.post(map[string]string{
		"chat_id": t.chatID,
		"text":    text,
	})
}

func (t *telegramChannel) sendHTML(html string) {
	t.post(map[string]string{
		"chat_id":    t.chatID,
		"text":       html,
		"parse_mode": "HTML",
	})
}

func (t *telegramChannel) post(payload map[string]string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
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
