#!/usr/bin/env python3
"""
official_collector.py — PARALLEL, OBSERVATION-ONLY sports feed.

Polls each league's OFFICIAL data API (MLB / NHL / NBA) and records live game
state into the `live_sports_official` table. This runs ALONGSIDE the existing
sports_collector.py (Polymarket feed → live_sports). It exists purely to gather
a few days of data so we can compare how far ahead the official feeds are vs the
Polymarket feed.

IMPORTANT: the trading bot does NOT read this table. Nothing here affects
trading. It is a measurement harness only. (Soccer + tennis are deliberately
out of scope for now — revisit separately.)

Sources (one request per source returns ALL current games):
  - Baseball:   https://statsapi.mlb.com/api/v1/schedule?sportId=1&hydrate=linescore
  - Hockey:     https://api-web.nhle.com/v1/score/now
  - Basketball: https://cdn.nba.com/static/json/liveData/scoreboard/todaysScoreboard_00.json

Run under pm2:
    pm2 start official_collector.py --name official-collector --interpreter python3
"""

import json
import os
import sqlite3
import subprocess
import time
from datetime import datetime, timedelta, timezone

DB_PATH = os.environ.get("DB_PATH", "/home/ubuntu/app/trades.db")
POLL_SECS = float(os.environ.get("OFFICIAL_POLL_SECS", "6"))
# A real browser UA — NBA's CDN blackholes non-browser/"bot" user-agents.
UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

_conn = None


def db():
    global _conn
    if _conn is None:
        _conn = sqlite3.connect(DB_PATH, timeout=30)
        _conn.execute(
            """
            CREATE TABLE IF NOT EXISTS live_sports_official (
                game_key    TEXT PRIMARY KEY,   -- e.g. "mlb:746789"
                source      TEXT,
                sport       TEXT,
                home_team   TEXT,
                away_team   TEXT,
                status      TEXT,
                score       TEXT,               -- "away-home"
                period      TEXT,               -- inning/period/quarter + clock
                live        INTEGER,
                ended       INTEGER,
                detail      TEXT,
                updated_at  TEXT NOT NULL        -- when WE observed it (UTC)
            )
            """
        )
        # Append-only timeline of state CHANGES from BOTH feeds, for correlation.
        # One row each time a game's (score, period) changes in a feed. Compare
        # the two feeds' rows for the same match_key to measure which led, by how
        # much. observed_at = when this logger saw it (uniform cadence for both);
        # feed_updated_at = the feed's own recorded time.
        _conn.execute(
            """
            CREATE TABLE IF NOT EXISTS feed_snapshots (
                id              INTEGER PRIMARY KEY AUTOINCREMENT,
                observed_at     TEXT NOT NULL,
                feed            TEXT,            -- 'polymarket' | 'official'
                source          TEXT,
                sport           TEXT,
                match_key       TEXT,            -- sport:date:teamA-teamB (sorted, normalized)
                away_team       TEXT,
                home_team       TEXT,
                score           TEXT,
                period          TEXT,
                feed_updated_at TEXT
            )
            """
        )
        _conn.execute("CREATE INDEX IF NOT EXISTS idx_fs_match ON feed_snapshots(match_key)")
        _conn.commit()
    return _conn


def get_json(url, timeout=12, extra_headers=None):
    # Fetch via curl (Python's urllib hangs on IPv6 routes from this host).
    #  -L         follow redirects (NHL's score endpoint 307-redirects)
    #  --http1.1  avoid HTTP/2 stream errors (NBA CDN throws curl error 92 on h2)
    cmd = ["curl", "-s", "-L", "--http1.1", "-m", str(int(timeout)),
           "-H", f"User-Agent: {UA}"]
    for k, v in (extra_headers or {}).items():
        cmd += ["-H", f"{k}: {v}"]
    cmd.append(url)
    out = subprocess.run(cmd, capture_output=True, timeout=timeout + 5)
    if out.returncode != 0:
        raise RuntimeError(f"curl exit {out.returncode}: {out.stderr.decode()[:120]}")
    return json.loads(out.stdout.decode())


def upsert(rows):
    if not rows:
        return
    now = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime())
    c = db()
    for r in rows:
        c.execute(
            """
            INSERT INTO live_sports_official
                (game_key, source, sport, home_team, away_team, status, score,
                 period, live, ended, detail, updated_at)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
            ON CONFLICT(game_key) DO UPDATE SET
                source=excluded.source, sport=excluded.sport,
                home_team=excluded.home_team, away_team=excluded.away_team,
                status=excluded.status, score=excluded.score, period=excluded.period,
                live=excluded.live, ended=excluded.ended, detail=excluded.detail,
                updated_at=excluded.updated_at
            """,
            (
                r["game_key"], r["source"], r["sport"], r["home_team"], r["away_team"],
                r["status"], r["score"], r["period"], r["live"], r["ended"],
                r.get("detail", ""), now,
            ),
        )
    c.commit()


# ── MLB (statsapi) ────────────────────────────────────────────────────────────
def poll_mlb():
    rows = []
    # Cover both UTC dates so US night games that roll past midnight UTC are caught.
    today = datetime.now(timezone.utc).date()
    dates = {today.isoformat(), (today - timedelta(days=1)).isoformat()}
    seen = set()
    for d in dates:
        url = f"https://statsapi.mlb.com/api/v1/schedule?sportId=1&date={d}&hydrate=linescore"
        data = get_json(url)
        for day in data.get("dates", []):
            for g in day.get("games", []):
                pk = g.get("gamePk")
                if pk is None or pk in seen:
                    continue
                seen.add(pk)
                st = g.get("status", {})
                abs_state = st.get("abstractGameState", "")  # Preview/Live/Final
                if abs_state == "Preview":
                    continue  # not started — skip noise
                ls = g.get("linescore", {}) or {}
                away = g.get("teams", {}).get("away", {})
                home = g.get("teams", {}).get("home", {})
                inning = ls.get("currentInning", "")
                half = ls.get("inningState", "")  # Top/Bottom/Middle/End
                rows.append({
                    "game_key": f"mlb:{pk}",
                    "source": "mlb-statsapi",
                    "sport": "Baseball",
                    "home_team": home.get("team", {}).get("name", ""),
                    "away_team": away.get("team", {}).get("name", ""),
                    "status": st.get("detailedState", abs_state),
                    "score": f"{away.get('score','')}-{home.get('score','')}",
                    "period": f"{half} {inning}".strip(),
                    "live": 1 if abs_state == "Live" else 0,
                    "ended": 1 if abs_state == "Final" else 0,
                    "detail": f"outs={ls.get('outs','')}",
                })
    return rows


# ── NHL (api-web) ─────────────────────────────────────────────────────────────
def poll_nhl():
    rows = []
    data = get_json("https://api-web.nhle.com/v1/score/now")
    for g in data.get("games", []):
        state = g.get("gameState", "")  # FUT/PRE/LIVE/CRIT/OFF/FINAL
        if state in ("FUT", "PRE"):
            continue
        away = g.get("awayTeam", {})
        home = g.get("homeTeam", {})
        clock = (g.get("clock") or {}).get("timeRemaining", "")
        rows.append({
            "game_key": f"nhl:{g.get('id')}",
            "source": "nhl-api",
            "sport": "Hockey",
            "home_team": home.get("abbrev", ""),
            "away_team": away.get("abbrev", ""),
            "status": state,
            "score": f"{away.get('score','')}-{home.get('score','')}",
            "period": f"P{g.get('period','')} {clock}".strip(),
            "live": 1 if state in ("LIVE", "CRIT") else 0,
            "ended": 1 if state in ("OFF", "FINAL") else 0,
            "detail": "",
        })
    return rows


# ── NBA (cdn liveData) ────────────────────────────────────────────────────────
def poll_nba():
    rows = []
    # NBA CDN returns 403 without a browser Referer/Origin.
    data = get_json(
        "https://cdn.nba.com/static/json/liveData/scoreboard/todaysScoreboard_00.json",
        extra_headers={"Referer": "https://www.nba.com/", "Origin": "https://www.nba.com"},
    )
    for g in data.get("scoreboard", {}).get("games", []):
        gs = g.get("gameStatus", 0)  # 1=pre, 2=live, 3=final
        if gs == 1:
            continue
        away = g.get("awayTeam", {})
        home = g.get("homeTeam", {})
        rows.append({
            "game_key": f"nba:{g.get('gameId')}",
            "source": "nba-cdn",
            "sport": "Basketball",
            "home_team": home.get("teamTricode", ""),
            "away_team": away.get("teamTricode", ""),
            "status": g.get("gameStatusText", "").strip(),
            "score": f"{away.get('score','')}-{home.get('score','')}",
            "period": f"Q{g.get('period','')} {g.get('gameClock','')}".strip(),
            "live": 1 if gs == 2 else 0,
            "ended": 1 if gs == 3 else 0,
            "detail": "",
        })
    return rows


SOURCES = [("mlb", poll_mlb), ("nhl", poll_nhl), ("nba", poll_nba)]


# ── Cross-feed correlation ────────────────────────────────────────────────────
# The Polymarket feed (live_sports) uses abbreviations (e.g. "LAA"); the MLB
# official feed uses full names ("Los Angeles Angels"). Map official MLB full
# names → Polymarket's abbreviation style so a match_key lines up across feeds.
# NHL/NBA: both sides are abbrev/tricode → uppercase compare (refine aliases
# once we see real games).
MLB_FULL_TO_ABBR = {
    "Arizona Diamondbacks": "ARI", "Athletics": "ATH", "Atlanta Braves": "ATL",
    "Baltimore Orioles": "BAL", "Boston Red Sox": "BOS", "Chicago Cubs": "CHC",
    "Chicago White Sox": "CHW", "Cincinnati Reds": "CIN", "Cleveland Guardians": "CLE",
    "Colorado Rockies": "COL", "Detroit Tigers": "DET", "Houston Astros": "HOU",
    "Kansas City Royals": "KC", "Los Angeles Angels": "LAA", "Los Angeles Dodgers": "LAD",
    "Miami Marlins": "MIA", "Milwaukee Brewers": "MIL", "Minnesota Twins": "MIN",
    "New York Mets": "NYM", "New York Yankees": "NYY", "Philadelphia Phillies": "PHI",
    "Pittsburgh Pirates": "PIT", "San Diego Padres": "SD", "San Francisco Giants": "SF",
    "Seattle Mariners": "SEA", "St. Louis Cardinals": "STL", "Tampa Bay Rays": "TB",
    "Texas Rangers": "TEX", "Toronto Blue Jays": "TOR", "Washington Nationals": "WSH",
}

# Last (score, period) seen per (feed, game) — so we only append on a change.
_last_state = {}


def normalize_team(name, feed, sport):
    n = (name or "").strip()
    if not n:
        return ""
    if sport == "baseball" and feed == "official":
        return MLB_FULL_TO_ABBR.get(n, n.upper())
    return n.upper()


def snapshot_feeds():
    """Append a row to feed_snapshots whenever a game's state changes in either
    feed. Observation-only — reads both tables, writes only feed_snapshots."""
    c = db()
    now = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime())
    date = now[:10]
    rows = []
    for gid, sport, away, home, score, period, upd in c.execute(
        "SELECT game_id, sport, away_team, home_team, score, period, updated_at "
        "FROM live_sports WHERE live=1 AND lower(sport) IN ('baseball','hockey','basketball')"
    ).fetchall():
        rows.append(("polymarket", "polymarket", str(gid), sport, away, home, score, period, upd))
    for gk, source, sport, away, home, score, period, upd in c.execute(
        "SELECT game_key, source, sport, away_team, home_team, score, period, updated_at "
        "FROM live_sports_official WHERE live=1"
    ).fetchall():
        rows.append(("official", source, gk, sport, away, home, score, period, upd))

    for feed, source, gid, sport, away, home, score, period, upd in rows:
        key = (feed, gid)
        state = (score, period)
        if _last_state.get(key) == state:
            continue  # unchanged since last cycle
        _last_state[key] = state
        sp = (sport or "").lower()
        mk = f"{sp}:{date}:" + "-".join(sorted([
            normalize_team(away, feed, sp), normalize_team(home, feed, sp),
        ]))
        c.execute(
            "INSERT INTO feed_snapshots (observed_at, feed, source, sport, match_key, "
            "away_team, home_team, score, period, feed_updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
            (now, feed, source, sp, mk, away, home, score, period, upd),
        )
    c.commit()


def main():
    print(f"[official] collector starting — DB={DB_PATH} poll={POLL_SECS}s", flush=True)
    db()
    while True:
        for name, fn in SOURCES:
            try:
                rows = fn()
                upsert(rows)
                live = sum(r["live"] for r in rows)
                if rows:
                    print(f"[official] {name}: {len(rows)} games ({live} live)", flush=True)
            except Exception as e:  # one source failing must not stop the others
                print(f"[official] {name} error: {e}", flush=True)
        # Correlate: append any state changes from BOTH feeds to feed_snapshots.
        try:
            snapshot_feeds()
        except Exception as e:
            print(f"[official] snapshot error: {e}", flush=True)
        time.sleep(POLL_SECS)


if __name__ == "__main__":
    main()
