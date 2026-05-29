#!/usr/bin/env python3
"""
sports_collector.py — live sports state sidecar for the Polymarket bot.

Holds a persistent websocket connection to Polymarket's sports feed
(wss://sports-api.polymarket.com/ws) and writes the current state of every
live game to the `live_sports` table in trades.db. The Go bot reads this
table to gate tennis entries by set (e.g. only trade in set 3 or end of set 2).

The sports stream pushes events immediately on connect and requires no
subscribe frame. The server sends "ping"; we reply "pong" to stay alive.

Run under pm2 alongside the bot:
    pm2 start sports_collector.py --name sports-collector --interpreter python3
"""

import json
import os
import sqlite3
import time

import websocket  # websocket-client

DB_PATH = os.environ.get("DB_PATH", "/home/ubuntu/app/trades.db")
WS_URL = "wss://sports-api.polymarket.com/ws"

_conn = None


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
    print("[sports] connected — streaming live game state", flush=True)


def main():
    print(f"[sports] collector starting — DB={DB_PATH}", flush=True)
    db()  # ensure table exists
    while True:
        try:
            ws = websocket.WebSocketApp(
                WS_URL,
                on_open=on_open,
                on_message=on_message,
                on_error=on_error,
                on_close=on_close,
            )
            # ping_interval keeps the TCP connection healthy; the app-level
            # ping/pong is handled in on_message.
            ws.run_forever(ping_interval=30, ping_timeout=10)
        except Exception as e:
            print(f"[sports] run_forever crashed: {e}", flush=True)
        # Reconnect after a short backoff.
        time.sleep(5)


if __name__ == "__main__":
    main()
