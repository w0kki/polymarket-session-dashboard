#!/usr/bin/env python3
"""
sports_collector.py — live sports state sidecar for the Polymarket bot.

Holds a persistent websocket connection to Polymarket's sports feed
(wss://sports-api.polymarket.com/ws) and writes the current state of every
live game to the `live_sports` table in trades.db. The Go bot reads this
table to gate entries by game state (e.g. tennis set, baseball inning).

The sports stream pushes events immediately on connect and requires no
subscribe frame. The server sends "ping"; we reply "pong" to stay alive.

Reliability (the Polymarket feed drops connections frequently):
  - Reconnect FAST after a normal drop (we had a live connection) so the
    staleness gap is ~1s, not 5s.
  - Exponential backoff ONLY when we can't establish a connection at all
    (immediate failure) so we don't hammer a down server.
  - A watchdog forces a reconnect if the socket is open but no message has
    arrived in STALE_SECS — catches silent stalls where the feed looks
    connected but has stopped delivering game state.

Run under pm2 alongside the bot:
    pm2 start sports_collector.py --name sports-collector --interpreter python3
"""

import json
import os
import random
import sqlite3
import threading
import time

import websocket  # websocket-client

DB_PATH = os.environ.get("DB_PATH", "/home/ubuntu/app/trades.db")
WS_URL = "wss://sports-api.polymarket.com/ws"

# Reconnect tuning.
BACKOFF_BASE = 1.0    # seconds — fast reconnect after a normal drop
BACKOFF_MAX = 30.0    # cap for repeated connect failures
STABLE_UPTIME = 5.0   # a connection that lived this long counts as "real"
STALE_SECS = 180.0    # force reconnect if no message arrives for this long. 60s
                      # was too aggressive for MLB — innings often have 60+s of
                      # pitcher delays / between-pitch quiet, causing needless
                      # reconnect churn. 180s tolerates normal game cadence
                      # while still recovering within ~3 min if the server
                      # genuinely stops pushing.

_conn = None

# Connection liveness state shared with the watchdog thread.
_last_msg = 0.0           # time.monotonic() of the most recent message
_current_ws = None        # the active WebSocketApp, for the watchdog to close
_ws_lock = threading.Lock()


def db():
    global _conn
    if _conn is None:
        _conn = sqlite3.connect(DB_PATH, timeout=30)
        _conn.execute(
            """
            CREATE TABLE IF NOT EXISTS live_sports (
                game_id     INTEGER PRIMARY KEY,
                league      TEXT,
                sport       TEXT,
                home_team   TEXT,
                away_team   TEXT,
                status      TEXT,
                score       TEXT,
                period      TEXT,
                live        INTEGER,
                ended       INTEGER,
                tournament  TEXT,
                updated_at  TEXT NOT NULL
            )
            """
        )
        _conn.commit()
    return _conn


def upsert(payload: dict):
    game_id = payload.get("gameId")
    if game_id is None:
        return
    state = payload.get("eventState") or {}
    sport = state.get("type", "")
    period = payload.get("period") or state.get("period") or ""
    score = payload.get("score") or state.get("score") or ""
    live = 1 if (payload.get("live") or state.get("live")) else 0
    ended = 1 if (payload.get("ended") or state.get("ended")) else 0
    tournament = state.get("tournamentName", "")

    now = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime())
    c = db()
    c.execute(
        """
        INSERT INTO live_sports
            (game_id, league, sport, home_team, away_team, status, score,
             period, live, ended, tournament, updated_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(game_id) DO UPDATE SET
            league=excluded.league, sport=excluded.sport,
            home_team=excluded.home_team, away_team=excluded.away_team,
            status=excluded.status, score=excluded.score, period=excluded.period,
            live=excluded.live, ended=excluded.ended,
            tournament=excluded.tournament, updated_at=excluded.updated_at
        """,
        (
            game_id,
            payload.get("leagueAbbreviation", ""),
            sport,
            payload.get("homeTeam", ""),
            payload.get("awayTeam", ""),
            payload.get("status", ""),
            score,
            period,
            live,
            ended,
            tournament,
            now,
        ),
    )
    c.commit()


def on_message(ws, msg):
    global _last_msg
    _last_msg = time.monotonic()
    if msg == "ping":
        ws.send("pong")
        return
    try:
        payload = json.loads(msg)
    except (ValueError, TypeError):
        return
    if isinstance(payload, dict) and payload.get("gameId") is not None:
        try:
            upsert(payload)
        except Exception as e:  # never let a DB hiccup kill the socket
            print(f"[sports] upsert error: {e}", flush=True)


def on_error(ws, error):
    print(f"[sports] ws error: {error}", flush=True)


def on_close(ws, code, reason):
    print(f"[sports] ws closed: {code} {reason}", flush=True)


def on_open(ws):
    global _last_msg
    _last_msg = time.monotonic()
    print("[sports] connected — streaming live game state", flush=True)


def watchdog():
    """Force a reconnect if the socket is open but silent (a stalled feed)."""
    while True:
        time.sleep(STALE_SECS / 3.0)
        with _ws_lock:
            ws = _current_ws
        if ws is None or _last_msg <= 0:
            continue
        silent = time.monotonic() - _last_msg
        if silent > STALE_SECS:
            print(
                f"[sports] no data for {silent:.0f}s — forcing reconnect (silent stall)",
                flush=True,
            )
            try:
                ws.close()
            except Exception:
                pass


def main():
    global _current_ws, _last_msg
    print(f"[sports] collector starting — DB={DB_PATH}", flush=True)
    db()  # ensure table exists
    threading.Thread(target=watchdog, daemon=True).start()

    backoff = BACKOFF_BASE
    while True:
        connected_at = time.monotonic()
        _last_msg = time.monotonic()
        try:
            ws = websocket.WebSocketApp(
                WS_URL,
                on_open=on_open,
                on_message=on_message,
                on_error=on_error,
                on_close=on_close,
            )
            with _ws_lock:
                _current_ws = ws
            # WS-level ping keeps TCP healthy; app-level ping/pong in on_message.
            ws.run_forever(ping_interval=20, ping_timeout=10)
        except Exception as e:
            print(f"[sports] run_forever crashed: {e}", flush=True)
        finally:
            with _ws_lock:
                _current_ws = None

        uptime = time.monotonic() - connected_at
        if uptime >= STABLE_UPTIME:
            # Normal drop after a real connection — reconnect fast.
            backoff = BACKOFF_BASE
        else:
            # Couldn't establish (or instant drop) — back off to avoid hammering.
            backoff = min(backoff * 2.0, BACKOFF_MAX)
        delay = backoff + random.uniform(0.0, backoff * 0.5)  # jitter
        print(
            f"[sports] disconnected after {uptime:.0f}s — reconnecting in {delay:.1f}s",
            flush=True,
        )
        time.sleep(delay)


if __name__ == "__main__":
    main()
