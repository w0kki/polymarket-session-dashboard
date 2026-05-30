package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// CommandHandler is called for each valid bot command received from Telegram.
// cmd is the command name without the leading slash (e.g. "status", "live").
// args is everything after the command, trimmed.
type CommandHandler func(cmd, args string)

// ListenCommands starts a long-poll loop that receives Telegram messages and
// calls handler for any command (text starting with '/') sent from the
// configured chat ID. Returns immediately if Telegram is not configured.
// Runs until ctx is cancelled.
func (n *Notifier) ListenCommands(ctx context.Context, handler CommandHandler) {
	if n.telegram == nil {
		log.Println("[cmd] Telegram not configured — command listener disabled")
		return
	}
	log.Println("[cmd] Telegram command listener active")
	go n.telegram.pollCommands(ctx, handler)
}

// ── Telegram long-polling ─────────────────────────────────────────────────────

type tgUpdate struct {
	UpdateID int        `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	Chat tgChat `json:"chat"`
	Text string `json:"text"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

func (t *telegramChannel) pollCommands(ctx context.Context, handler CommandHandler) {
	// Use a separate client with a longer timeout for long-polling.
	client := &http.Client{Timeout: 35 * time.Second}
	offset := 0

	// Drain any backlog on startup WITHOUT processing it. Otherwise a restart
	// re-fetches old un-acknowledged commands (offset resets to 0) and replays
	// them — and a queued /startup would re-trigger a restart, looping forever.
	if _, lastID, err := t.getUpdates(ctx, client, -1, 0); err == nil && lastID > 0 {
		offset = lastID + 1
		log.Printf("[cmd] drained command backlog on startup (offset → %d)", offset)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, lastID, err := t.getUpdates(ctx, client, offset, 25)
		if err != nil {
			// Context cancelled — exit cleanly.
			if ctx.Err() != nil {
				return
			}
			log.Printf("[cmd] getUpdates error: %v — retrying in 5s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		for _, upd := range updates {
			if upd.Message == nil {
				continue
			}
			// Security: only accept messages from the configured chat.
			if fmt.Sprint(upd.Message.Chat.ID) != t.chatID {
				log.Printf("[cmd] ignoring message from unknown chat %d", upd.Message.Chat.ID)
				continue
			}
			text := strings.TrimSpace(upd.Message.Text)
			if !strings.HasPrefix(text, "/") {
				continue
			}
			// Strip any @BotName suffix Telegram adds in group chats.
			text = strings.SplitN(text, "@", 2)[0]

			parts := strings.SplitN(strings.TrimPrefix(text, "/"), " ", 2)
			cmd := strings.ToLower(strings.TrimSpace(parts[0]))
			args := ""
			if len(parts) > 1 {
				args = strings.TrimSpace(parts[1])
			}
			log.Printf("[cmd] received /%s %s", cmd, args)
			handler(cmd, args)
		}

		if lastID > 0 {
			offset = lastID + 1
		}
	}
}

func (t *telegramChannel) getUpdates(ctx context.Context, client *http.Client, offset, timeoutSec int) ([]tgUpdate, int, error) {
	url := fmt.Sprintf(
		"https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=%d&allowed_updates=[\"message\"]",
		t.token, offset, timeoutSec,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	var result struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, 0, fmt.Errorf("parse getUpdates: %w", err)
	}
	if !result.OK {
		return nil, 0, fmt.Errorf("getUpdates not ok: %s", body)
	}

	lastID := 0
	for _, u := range result.Result {
		if u.UpdateID > lastID {
			lastID = u.UpdateID
		}
	}
	return result.Result, lastID, nil
}
