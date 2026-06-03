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
	mode     string // "PAPER" or "LIVE" — shown in every trade notification
}

// New constructs a Notifier. Pass empty strings to disable individual channels.
// dryRun=true → mode label "PAPER"; false → "LIVE".
func New(discordWebhookURL, telegramToken, telegramChatID string, dryRun bool) *Notifier {
	mode := "LIVE"
	if dryRun {
		mode = "PAPER"
	}
	client := &http.Client{Timeout: 10 * time.Second}
	n := &Notifier{client: client, mode: mode}
	if discordWebhookURL != "" {
		n.discord = &discordChannel{url: discordWebhookURL, client: client}
	}
	if telegramToken != "" && telegramChatID != "" {
		n.telegram = &telegramChannel{token: telegramToken, chatID: telegramChatID, client: client}
	}
	return n
}

// SetMode updates the mode label shown in trade notifications.
// Call this after a mode_override is applied so "PAPER" / "LIVE" is accurate.
func (n *Notifier) SetMode(dryRun bool) {
	if dryRun {
		n.mode = "PAPER"
	} else {
		n.mode = "LIVE"
	}
}

// Enabled reports whether at least one notification channel is active.
func (n *Notifier) Enabled() bool {
	return n.discord != nil || n.telegram != nil
}

// Broadcast sends plain text to all channels. Public alias used by the
// Telegram command handler so it can send replies without importing internals.
func (n *Notifier) Broadcast(plain string) { n.broadcast(plain) }

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
		"📝 **TRADE PLACED** — %s · Moneyline · %s\n[%s](%s)\n▶ %s @ %.1f¢ | Size: $%.2f",
		n.mode, sport, market, url, side, price*100, size,
	)
	telegram := fmt.Sprintf(
		"📝 <b>TRADE PLACED</b> — %s · Moneyline · %s\n<a href=\"%s\">%s</a>\n▶ %s @ %.1f¢ | Size: $%.2f",
		n.mode, sport, url, market, side, price*100, size,
	)
	if url == "" {
		n.broadcast(fmt.Sprintf("📝 TRADE PLACED — %s · Moneyline · %s\n%s\n▶ %s @ %.1f¢ | Size: $%.2f",
			n.mode, sport, market, side, price*100, size))
		return
	}
	n.broadcastLink(discord, telegram)
}

// TradeResolved fires when a trade settles as WIN or LOSS.
func (n *Notifier) TradeResolved(market, side, outcome, slug string, pnl float64) {
	url := polyURL(slug)
	switch {
	case outcome == "LOSS":
		discord := fmt.Sprintf("📉 **LOSS** — %s · Moneyline\n[%s](%s)\n▶ %s | P&L: -$%.2f", n.mode, market, url, side, -pnl)
		telegram := fmt.Sprintf("📉 <b>LOSS</b> — %s · Moneyline\n<a href=\"%s\">%s</a>\n▶ %s | P&L: -$%.2f", n.mode, url, market, side, -pnl)
		if url == "" {
			n.broadcast(fmt.Sprintf("📉 LOSS — %s · %s\n%s\nP&L: -$%.2f", n.mode, side, market, -pnl))
			return
		}
		n.broadcastLink(discord, telegram)
	case outcome == "WIN":
		discord := fmt.Sprintf("✅ **WIN** — %s · Moneyline\n[%s](%s)\n▶ %s | P&L: +$%.2f", n.mode, market, url, side, pnl)
		telegram := fmt.Sprintf("✅ <b>WIN</b> — %s · Moneyline\n<a href=\"%s\">%s</a>\n▶ %s | P&L: +$%.2f", n.mode, url, market, side, pnl)
		if url == "" {
			n.broadcast(fmt.Sprintf("✅ WIN — %s · %s\n%s\nP&L: +$%.2f", n.mode, side, market, pnl))
			return
		}
		n.broadcastLink(discord, telegram)
	}
}

// StopLossTriggered fires when a position is exited early via stop loss.
func (n *Notifier) StopLossTriggered(market, side, sport, slug string, exitPrice, netPnl, saved float64) {
	url := polyURL(slug)
	discord := fmt.Sprintf(
		"⛔ **STOP LOSS** — %s · Moneyline · %s\n[%s](%s)\n▶ %s exited @ %.1f¢ | Loss: -$%.2f | Saved $%.2f vs full loss",
		n.mode, sport, market, url, side, exitPrice*100, -netPnl, saved,
	)
	telegram := fmt.Sprintf(
		"⛔ <b>STOP LOSS</b> — %s · Moneyline · %s\n<a href=\"%s\">%s</a>\n▶ %s exited @ %.1f¢ | Loss: -$%.2f | Saved $%.2f vs full loss",
		n.mode, sport, url, market, side, exitPrice*100, -netPnl, saved,
	)
	if url == "" {
		n.broadcast(fmt.Sprintf("⛔ STOP LOSS — %s · %s\n%s\n▶ %s exited @ %.1f¢ | Loss: -$%.2f | Saved $%.2f",
			n.mode, sport, market, side, exitPrice*100, -netPnl, saved))
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
	payload := map[string]interface{}{
		"content": content,
		"flags":   4, // SUPPRESS_EMBEDS
	}
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
	t.post(map[string]interface{}{
		"chat_id":                  t.chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
}

func (t *telegramChannel) sendHTML(html string) {
	t.post(map[string]interface{}{
		"chat_id":                  t.chatID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
}

func (t *telegramChannel) post(payload map[string]interface{}) {
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
