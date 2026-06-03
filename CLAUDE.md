# Polymarket Sports Bot + Dashboard — CLAUDE.md

> Last substantial update: 2026-06-02. This file is the source of truth for how the
> system actually runs today. If you change behavior, update this file.

## ⚠️ Read this first

- **The bot trades REAL MONEY (live).** Despite `DRY_RUN: "true"` in `ecosystem.config.cjs`,
  the bot runs **LIVE** because the SQLite `settings` table has `mode_override = 'live'`
  (set via the Telegram `/live` command). The DB override wins over the env var. Do **not**
  assume paper mode from the ecosystem file — check `settings.mode_override`.
- **Secrets live in `ecosystem.config.cjs` on the server** (private key, API creds, Telegram
  token). That file is `.gitignore`d and must never be committed.
- **Strategy edge is currently NEGATIVE** (see "Performance reality"). Treat it as an
  experiment under active tuning, not a money printer.

---

## What this is

A single project running a live Polymarket sports-betting bot plus a web dashboard, sharing
one SQLite database. The bot backs **heavy favorites at high prices** in live sports markets
("buy near $1, hold to settlement" — a risk-premia / "bonding" trade).

Four processes (all under **pm2** on one EC2 box):

| pm2 name | Entry | Role |
|---|---|---|
| `polymarket-bot` | `bot/bot-ec2` (Go) | The trading bot — discovery, gates, sizing, execution, safety nets |
| `trading-dashboard` | `server.js` (Node/Express) | REST API + serves the React dashboard + hourly wallet sync (`sync.js`) |
| `sports-collector` | `bot/sports_collector.py` | Polymarket WS game-state feed → `live_sports` table. **Drives the game gates.** |
| `official-collector` | `bot/official_collector.py` | Official MLB/NHL/NBA feeds → `live_sports_official` + `feed_snapshots`. **OBSERVATION-ONLY — not used for trading.** |

---

## Server / access

- **Host:** `ubuntu@18.203.230.72` (AWS EC2, eu-west-1, x86-64)
- **SSH key:** `~/.ssh/yfm-polly1-key.pem`
- **App dir:** `/home/ubuntu/app/`  · **Bot dir:** `/home/ubuntu/app/bot/`  · **DB:** `/home/ubuntu/app/trades.db`
- **pm2 logs:** `pm2 logs <name> --lines 50 --nostream`

```bash
ssh -i ~/.ssh/yfm-polly1-key.pem ubuntu@18.203.230.72
```

---

## Repository layout

```
polymarket-session-dashboard/
├── server.js              # Express: REST API, serves dist/, runs runSync() hourly
├── sync.js                # Polymarket wallet → SQLite sync (positions + activity)
├── db.js                  # better-sqlite3 wrapper (the trades upsert lives here)
├── src/                   # React 19 + TS + Vite 6 + Tailwind 4 frontend
│   ├── App.tsx            # dashboard views + KPI cards
│   ├── lib/polymarket.ts  # client data shaping (buildTradeRows, computeStats, detectSport)
│   └── types.ts
├── dist/                  # built frontend (served statically; deploy artifact)
├── trades.db              # SQLite — shared by bot + dashboard (NOT committed)
└── bot/                   # Go module: github.com/w0kki/polymarket-bot (Go 1.22)
    ├── cmd/main.go        # run loops, gates, safety nets, Telegram command handler
    ├── internal/
    │   ├── config/        # env-var config + defaults (config.go)
    │   ├── market/        # scanner.go — CLOB scan, slug→sport map, gates, live book pricing
    │   ├── kelly/         # half-Kelly sizing
    │   ├── db/            # SQLite queries (live-trade stats, GetLiveSport, settings)
    │   ├── executor/      # executor.go (iface) · live.go (CLOB V2 signing) · paper
    │   └── notify/        # discord.go · commands.go (Telegram long-poll)
    ├── sports_collector.py     # Polymarket WS sidecar  → live_sports
    ├── official_collector.py   # MLB/NHL/NBA sidecar    → live_sports_official, feed_snapshots
    ├── ecosystem.config.cjs    # pm2 config + ALL env/secrets (server-only, gitignored)
    └── go.mod             # modernc.org/sqlite (pure Go, no CGO) + go-ethereum (EIP-712)
```

---

## The strategy

Back the heavy favorite in a live binary sports market at a high price and hold to settlement.
Per-sport **price band** + a **game-state gate** (only enter once a game is "decided enough").

### Per-sport price bands (env)
| Sport | Min | Max |
|---|---|---|
| Tennis | 95¢ | 97¢ |
| Baseball | 94¢ | **99¢** |
| Hockey | 95¢ | 97¢ |
| Soccer | 94¢ | 97¢ (final-30-min gate — **currently broken**, see Known issues) |

### Game-state gates (from `live_sports`, fail-closed: no fresh data ⇒ skip)
| Sport | Enter when… |
|---|---|
| Baseball | 8th inning+ **OR** run diff ≥ 6 |
| Hockey | 2nd period+ **OR** goal diff ≥ 5 |
| Basketball | 4th quarter+ **OR** point diff ≥ 25 |
| Tennis | set 2+ (or end of set 1) |

### Leagues traded (slug-prefix → sport map in `scanner.go`, mirrored in `polymarket.ts`)
- **Soccer:** epl, lal, fl1, fl2, spl, mls, bun, ser, ucl, uel, wc, afc, csl, ksl, jsl, asl, bra, tur, nor, fif
- **Baseball:** mlb, kbo · **Basketball:** nba, wnba · **Hockey:** nhl · **Tennis:** atp, wta · (nfl/ufc mapped but not enabled)
- Only sports in `SPORTS` env actually trade. Doubles are excluded from live (see below).

### Sizing
Half-Kelly off the bankroll, capped at `MAX_POSITION_SIZE`. **The live edge is non-positive, so
Kelly returns the `FALLBACK_SIZE` ($100) on essentially every trade.** Kelly stats use **live
trades only** (`trade_type IN ('Risk Premia','Latency Arb')`); paper trades are excluded.

### Doubles experiment
Tennis doubles are excluded from live trading. With `PAPER_TRADE_DOUBLES=true`, doubles are
allowed onto the watchlist tagged `PaperOnly` and routed to the **paper** executor (recorded
`trade_type='Paper'`) — never touching live capital. Used to evaluate if they're worth re-enabling.

---

## Run loops (in `cmd/main.go`)

Three independent tickers; the poll and stop-loss run as guarded goroutines so neither blocks the other:

| Loop | Interval (env) | Does |
|---|---|---|
| Discovery | `SCAN_INTERVAL_SEC` = 150s | Rebuild watchlist, resolve settled trades |
| Poll | `POLL_INTERVAL_SEC` = 5s | Price-check watchlist, apply gates, place entries. Requests are **staggered** across the interval to avoid CLOB 429s. |
| Stop-loss | `STOP_LOSS_INTERVAL_SEC` = 3s | Dedicated, decoupled from the entry scan so exits react fast |

Pricing uses the **live order-book best ask** (CLOB `/book`), not the stale `token.price`.

---

## Stop-loss (per-sport)

- Global `STOP_LOSS_DROP=0.40` (exit if price falls 40¢ from entry). Per-sport overrides via
  `<SPORT>_STOP_LOSS_DROP`; **0 disables it** for that sport.
- **`TENNIS_STOP_LOSS_DROP=0` → tennis holds to settlement.** (Tennis favorites swing wildly on
  a lost set and usually recover; the stop was selling eventual winners — "whipsaw".)
- **No-liquidity write-off (💀):** when a backed favorite collapses to ≤5¢ with an empty book,
  the FAK sell can't fill. The bot records the realized loss once and stops retrying (instead of
  hammering an unfillable sell every cycle).
- The bot's `STOP_LOSS` records are authoritative — `sync.js` must NOT overwrite them with
  settlement values (it preserves them now).

---

## Safety nets (`checkSafetyNets` in main.go)

| Net | Mechanism |
|---|---|
| **Bankroll floor** | `floor = bankroll × BANKROLL_FLOOR_PCT (0.50)`. Compared against the **real on-chain balance** = cash (CLOB `/balance-allowance`, `signature_type=3`) + open-position value (data-api `/value`). Requires **3 consecutive breached readings** to act (guards against transient feed glitches), then sets `bot_killed` and restarts into dormant mode — does **not** crash-loop. If the balance lookup fails, the check is skipped that cycle (never halt on a fetch error). |
| **Daily loss limit** | `MAX_DAILY_LOSS` ($300). Keyed on **resolution date** (`date(updated_at)`), live-only. |
| **Circuit breaker** | `CONSEC_LOSS_LIMIT` (5) consecutive **live** losses → pause. |
| **`/startup` override** | Sets `safety_override = <today UTC>`, which bypasses circuit breaker + daily loss **for that day only** — but **NOT** the bankroll floor. |

Dormant mode (`bot_killed='true'`): process stays up under pm2 but only the Telegram listener
runs (no trading). Resume with `/startup`.

---

## Live execution (`executor/live.go`)

- Polymarket **CLOB V2**, **POLY_1271 / `signatureType=3`**: the **deposit (proxy) wallet** is
  both maker and signer; orders are ERC-7739 **TypedDataSign**-wrapped, signed with the EOA key.
- **FAK** (Fill-And-Kill) orders. Decimal precision is by field: **makerAmount ≤2 dp** (÷10000
  µ-units), **takerAmount ≤4 dp** (÷100). Getting these backwards = "invalid amounts".
- On-chain confirmation via data-api `/positions` (`confirmFill` after buy, `confirmExit` after
  sell); returns `ErrOrderUnconfirmed` if a "delayed" order never actually fills. Sells are capped
  to the wallet's actual on-chain position size.
- `POLY_PROXY_WALLET` = deposit wallet `0x8aB08a84e5a64Bd46B62aD402a090971147350E3`.

---

## Telegram commands (`notify/commands.go` + handler in main.go)

`/status` · `/bankroll <n>` · `/stoploss <cents>` · `/fallback <dollars>` · `/kill` ·
`/startup` (resume + clear breaker/daily-halt for the day) · `/live` · `/paper` · `/clearbreaker`

Most settings are read live from the DB each cycle, so they take effect **without a restart**.
On boot the listener drains the command backlog so a queued `/startup` can't cause a restart loop.

---

## Database (`trades.db`, shared)

| Table | Purpose |
|---|---|
| `trades` | Live + paper trades. `trade_type`: `Risk Premia`/`Latency Arb` = live, `Paper` = paper. `outcome`: WIN/LOSS/STOP_LOSS/NA. Key cols: entry_price, exit_price, size_usdc, net_pnl, side, slug, sport, `updated_at` (resolution time). |
| `settings` | Key-value: `mode_override`, `bankroll`, `bot_killed`, `safety_override`, `circuit_breaker_until`, `daily_loss_halt`, `stop_loss_drop`, `fallback_size`, `last_balance`. |
| `live_sports` | Polymarket WS game state (period, score, live, ended) — read by the gates. |
| `live_sports_official` | MLB/NHL/NBA official state — **eval only**, not read by the bot. |
| `feed_snapshots` | Append-only timeline of both feeds' state changes (for latency comparison). |
| `positions` | Open wallet positions (dashboard sync). |

**Data-integrity notes (fixed 2026-06-02):** `sync.js`/`db.js` no longer re-stamp `updated_at`
on every hourly sync (it was double-counting old P&L in time-windowed queries), and it preserves
`STOP_LOSS` records instead of overwriting them with settlement values.

---

## Deploying

### Bot (Go)
```bash
cd bot
GOOS=linux GOARCH=amd64 go build -o /tmp/bot-ec2 ./cmd        # cross-compile (x86-64)
scp -i ~/.ssh/yfm-polly1-key.pem /tmp/bot-ec2 ubuntu@18.203.230.72:/home/ubuntu/app/bot/bot-ec2.new
ssh -i ~/.ssh/yfm-polly1-key.pem ubuntu@18.203.230.72 \
  'cd /home/ubuntu/app/bot && cp bot-ec2 bot-ec2.bak && mv bot-ec2.new bot-ec2 && chmod +x bot-ec2 && \
   pm2 restart polymarket-bot --update-env && pm2 save'
```

### Dashboard (frontend)
```bash
npm run build        # tsc -b && vite build → dist/
scp -i ~/.ssh/yfm-polly1-key.pem dist/index.html ubuntu@18.203.230.72:/home/ubuntu/app/dist/
scp -i ~/.ssh/yfm-polly1-key.pem dist/assets/* ubuntu@18.203.230.72:/home/ubuntu/app/dist/assets/
# static files — no restart needed; hard-refresh the browser
```

### Config / env changes
Edit `ecosystem.config.cjs` **on the server**, then:
```bash
pm2 restart ecosystem.config.cjs --update-env && pm2 save
```
The Python collectors and the dashboard are separate pm2 apps; restart them by name as needed
(`pm2 restart sports-collector` / `official-collector` / `trading-dashboard`).

### Local dev (dashboard only)
```bash
npm install && npm run dev      # Vite hot-reload frontend (port 5174); no backend
npm start                       # node server.js (API + serves dist/)
```

---

## Bot env reference (set in `ecosystem.config.cjs → env`)

| Var | Current | Notes |
|---|---|---|
| `DRY_RUN` | `true` | **Overridden to LIVE by `settings.mode_override`** |
| `DB_PATH` | `/home/ubuntu/app/trades.db` | |
| `SCAN_INTERVAL_SEC` / `POLL_INTERVAL_SEC` / `STOP_LOSS_INTERVAL_SEC` | 150 / 5 / 3 | discovery / poll / stop-loss |
| `MAX_POSITION_SIZE` / `FALLBACK_SIZE` | 200 / 100 | cap / Kelly fallback |
| `BANKROLL_FLOOR_PCT` | 0.50 | floor = bankroll × this |
| `MAX_DAILY_LOSS` / `CONSEC_LOSS_LIMIT` | (default 300) / 5 | safety nets |
| `SPORTS` | Baseball,Tennis,Basketball,Hockey,Soccer | enabled sports |
| `MIN_VOLUME` | 30000 | min market $ volume (fail-closed) |
| `<SPORT>_MIN_PRICE` / `_MAX_PRICE` | see bands above | per-sport price window |
| `<SPORT>_MIN_INNING/PERIOD/QUARTER/SET` + `_RUN/GOAL/POINT_DIFF` | see gates above | game-state gates |
| `STOP_LOSS_DROP` / `TENNIS_STOP_LOSS_DROP` | 0.40 / 0 | global stop / tennis disabled |
| `PAPER_TRADE_DOUBLES` | true | route doubles to paper |
| `POLY_*` / `TELEGRAM_*` / `DISCORD_WEBHOOK_URL` | (secrets) | live creds + alerts |

---

## Performance reality (as of 2026-06-02)

- Live edge is **negative**: ~91–92% win rate vs the **~93% breakeven** implied by the ~11–14:1
  loss asymmetry (avg win ~$2.5 vs avg loss ~$30). Lots of small wins, periodically undone by one
  big loss. Small position size is the main thing keeping it survivable.
- **Baseball is the worst sport** (~80% of net losses) — late-inning collapses/walk-offs on
  favorites you can't stop out of (empty book). Hence the stricter 8th-inning gate + 99¢ ceiling.
- **Tennis losses were largely self-inflicted whipsaws** (stopping out winners who recovered) →
  tennis stop-loss disabled.

---

## Known issues / open follow-ups (also tracked in agent memory)

1. **Soccer "final 30 min" gate is broken** — keys off `end_date_iso`, which Polymarket sets to
   midnight of the match date (not the real match clock), so it never enforces the window. Needs
   a real match clock from the sports feed.
2. **Tennis stop-loss disabled** — review ~2026-06-09 whether holding-to-settlement helped.
3. **Baseball gate → official MLB feed** — gate on outs-aware MLB Stats API state ("home leading
   after top 9th" / "2 outs bottom 9th lead ≥4") instead of the laggy Polymarket inning+score.
4. **Feed-latency evaluation** — Polymarket's game feed lags real time (confirmed: a tennis
   tiebreak showed 0-0 vs an actual 6-2). `official_collector` + `feed_snapshots` are gathering
   data to decide whether to switch feeds. **Tennis has no free official feed (the gap).**
5. **Paper-doubles experiment** — review ~2026-06-09.
6. Historical trade records pre-dating the game-ID/ledger fixes are noisy; reconcile against
   official game IDs if doing precise historical analysis.
```
