/**
 * db.js — SQLite database module
 *
 * Designed for easy migration to PostgreSQL:
 *   - Standard SQL only (no SQLite-specific syntax in queries)
 *   - Replace `new Database(path)` with `new Pool({ connectionString })` and adapt the API
 *   - All queries use named parameters
 */

import Database from 'better-sqlite3';
import { join } from 'path';

const DB_PATH = join(import.meta.dirname, 'trades.db');
const db = new Database(DB_PATH);

// WAL mode: faster writes, safe concurrent reads
db.pragma('journal_mode = WAL');
db.pragma('foreign_keys = ON');

// ─── Schema ──────────────────────────────────────────────────────────────────

db.exec(`
  CREATE TABLE IF NOT EXISTS trades (
    condition_id   TEXT    PRIMARY KEY,
    date           TEXT,
    market         TEXT,
    slug           TEXT,
    sport          TEXT,
    trade_type     TEXT,
    side           TEXT,
    entry_price    REAL,
    shares         REAL,
    size_usdc      REAL,
    exit_price     REAL,
    outcome        TEXT,
    pnl            REAL,
    pnl_pct        REAL,
    fee_cat        TEXT,
    buy_fee        REAL,
    sell_fee       REAL,
    total_fees     REAL,
    net_pnl        REAL,
    icon           TEXT,
    first_seen_at  TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at     TEXT    NOT NULL DEFAULT (datetime('now'))
  );

  CREATE TABLE IF NOT EXISTS positions (
    condition_id   TEXT    PRIMARY KEY,
    title          TEXT,
    outcome        TEXT,
    size           REAL,
    avg_price      REAL,
    cur_price      REAL,
    initial_value  REAL,
    current_value  REAL,
    cash_pnl       REAL,
    percent_pnl    REAL,
    icon           TEXT,
    slug           TEXT,
    end_date       TEXT,
    redeemable     INTEGER,
    updated_at     TEXT    NOT NULL DEFAULT (datetime('now'))
  );

  CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
  );

  CREATE TABLE IF NOT EXISTS sync_log (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    synced_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    trades_count   INTEGER NOT NULL DEFAULT 0,
    positions_count INTEGER NOT NULL DEFAULT 0,
    status         TEXT    NOT NULL DEFAULT 'ok',
    error_msg      TEXT
  );

  CREATE INDEX IF NOT EXISTS idx_trades_date    ON trades(date);
  CREATE INDEX IF NOT EXISTS idx_trades_sport   ON trades(sport);
  CREATE INDEX IF NOT EXISTS idx_trades_outcome ON trades(outcome);
  CREATE INDEX IF NOT EXISTS idx_trades_type    ON trades(trade_type);
`);

// ─── Trades ──────────────────────────────────────────────────────────────────

const upsertTrade = db.prepare(`
  INSERT INTO trades (
    condition_id, date, market, slug, sport, trade_type, side,
    entry_price, shares, size_usdc, exit_price, outcome,
    pnl, pnl_pct, fee_cat, buy_fee, sell_fee, total_fees, net_pnl, icon, updated_at
  ) VALUES (
    @condition_id, @date, @market, @slug, @sport, @trade_type, @side,
    @entry_price, @shares, @size_usdc, @exit_price, @outcome,
    @pnl, @pnl_pct, @fee_cat, @buy_fee, @sell_fee, @total_fees, @net_pnl, @icon, datetime('now')
  )
  ON CONFLICT(condition_id) DO UPDATE SET
    date         = excluded.date,
    market       = excluded.market,
    -- Authoritative-record rules. Two cases where the BOT, not sync, owns the
    -- numbers and sync must NOT overwrite them:
    --   Paper      — the bot owns paper outcomes entirely.
    --   STOP_LOSS  — the bot actually SOLD the position early at a known price,
    --                so its exit_price / P&L are real. The market's eventual
    --                settlement is irrelevant to what we realized. (Previously
    --                sync reclassified stop-loss exits to WIN/LOSS with
    --                settlement values, corrupting the realized P&L.)
    -- Otherwise: never overwrite a resolved live outcome with 'NA' (the bot may
    -- have resolved it before the hourly sync); accept any definitive sync value.
    exit_price   = CASE
                     WHEN trades.trade_type = 'Paper'    THEN trades.exit_price
                     WHEN trades.outcome   = 'STOP_LOSS' THEN trades.exit_price
                     ELSE excluded.exit_price
                   END,
    outcome      = CASE
                     WHEN trades.trade_type = 'Paper'                        THEN trades.outcome
                     WHEN trades.outcome   = 'STOP_LOSS'                     THEN trades.outcome
                     WHEN trades.outcome != 'NA' AND excluded.outcome = 'NA' THEN trades.outcome
                     ELSE excluded.outcome
                   END,
    pnl          = CASE
                     WHEN trades.trade_type = 'Paper'                        THEN trades.pnl
                     WHEN trades.outcome   = 'STOP_LOSS'                     THEN trades.pnl
                     WHEN trades.outcome != 'NA' AND excluded.outcome = 'NA' THEN trades.pnl
                     ELSE excluded.pnl
                   END,
    pnl_pct      = CASE
                     WHEN trades.trade_type = 'Paper'                        THEN trades.pnl_pct
                     WHEN trades.outcome   = 'STOP_LOSS'                     THEN trades.pnl_pct
                     WHEN trades.outcome != 'NA' AND excluded.outcome = 'NA' THEN trades.pnl_pct
                     ELSE excluded.pnl_pct
                   END,
    net_pnl      = CASE
                     WHEN trades.trade_type = 'Paper'                        THEN trades.net_pnl
                     WHEN trades.outcome   = 'STOP_LOSS'                     THEN trades.net_pnl
                     WHEN trades.outcome != 'NA' AND excluded.outcome = 'NA' THEN trades.net_pnl
                     ELSE excluded.net_pnl
                   END,
    sell_fee     = CASE
                     WHEN trades.trade_type = 'Paper'    THEN trades.sell_fee
                     WHEN trades.outcome   = 'STOP_LOSS' THEN trades.sell_fee
                     ELSE excluded.sell_fee
                   END,
    total_fees   = CASE
                     WHEN trades.trade_type = 'Paper'    THEN trades.total_fees
                     WHEN trades.outcome   = 'STOP_LOSS' THEN trades.total_fees
                     ELSE excluded.total_fees
                   END,
    -- updated_at is a STABLE resolution timestamp: bump it ONLY when this sync
    -- actually resolves a live trade (NA → final outcome). Previously this was an
    -- unconditional datetime('now'), so every hourly sync re-stamped every already
    -- resolved trade — making old wins/losses re-appear inside time-windowed P&L
    -- queries (GetLivePnLSince, the daily-loss limit) and double-counting them.
    updated_at   = CASE
                     WHEN trades.trade_type != 'Paper'
                       AND trades.outcome = 'NA' AND excluded.outcome != 'NA'
                     THEN datetime('now')
                     ELSE trades.updated_at
                   END
`);

export const upsertTrades = db.transaction((rows) => {
  for (const row of rows) upsertTrade.run(row);
});

export function getTrades({ sport, outcome, from, to, limit = 500 } = {}) {
  let sql = 'SELECT * FROM trades WHERE 1=1';
  const params = [];
  if (sport)   { sql += ' AND sport = ?';      params.push(sport); }
  if (outcome) { sql += ' AND outcome = ?';    params.push(outcome); }
  if (from)    { sql += ' AND date >= ?';      params.push(from); }
  if (to)      { sql += ' AND date <= ?';      params.push(to); }
  // first_seen_at gives true chronological order; date is the match date only
  // (no time) so it can't disambiguate same-day trades on its own.
  sql += ' ORDER BY date ASC, first_seen_at ASC LIMIT ?';
  params.push(limit);
  return db.prepare(sql).all(...params);
}

export function getTradeStats() {
  return db.prepare(`
    SELECT
      COUNT(*)                                                          AS total_trades,
      SUM(CASE WHEN outcome = 'WIN'                    THEN 1 ELSE 0 END) AS wins,
      SUM(CASE WHEN outcome IN ('LOSS','STOP_LOSS')    THEN 1 ELSE 0 END) AS losses,
      SUM(CASE WHEN outcome = 'STOP_LOSS'              THEN 1 ELSE 0 END) AS stop_losses,
      ROUND(SUM(COALESCE(net_pnl, pnl)), 4)                           AS total_pnl,
      ROUND(SUM(net_pnl), 4)                                          AS total_net_pnl,
      ROUND(SUM(total_fees), 4)                                       AS total_fees,
      ROUND(AVG(CASE WHEN outcome = 'WIN'                 THEN net_pnl END), 4) AS avg_win,
      ROUND(AVG(CASE WHEN outcome IN ('LOSS','STOP_LOSS') THEN net_pnl END), 4) AS avg_loss,
      ROUND(MAX(CASE WHEN outcome = 'WIN'                 THEN net_pnl END), 4) AS largest_win,
      ROUND(MIN(CASE WHEN outcome IN ('LOSS','STOP_LOSS') THEN net_pnl END), 4) AS largest_loss,
      sport,
      COUNT(*) AS trades_by_sport
    FROM trades
    WHERE outcome IN ('WIN','LOSS','STOP_LOSS')
  `).get();
}

// ─── Positions ───────────────────────────────────────────────────────────────

const upsertPosition = db.prepare(`
  INSERT INTO positions (
    condition_id, title, outcome, size, avg_price, cur_price,
    initial_value, current_value, cash_pnl, percent_pnl,
    icon, slug, end_date, redeemable, updated_at
  ) VALUES (
    @condition_id, @title, @outcome, @size, @avg_price, @cur_price,
    @initial_value, @current_value, @cash_pnl, @percent_pnl,
    @icon, @slug, @end_date, @redeemable, datetime('now')
  )
  ON CONFLICT(condition_id) DO UPDATE SET
    title         = excluded.title,
    size          = excluded.size,
    cur_price     = excluded.cur_price,
    current_value = excluded.current_value,
    cash_pnl      = excluded.cash_pnl,
    percent_pnl   = excluded.percent_pnl,
    redeemable    = excluded.redeemable,
    updated_at    = datetime('now')
`);

export const upsertPositions = db.transaction((rows) => {
  for (const row of rows) upsertPosition.run(row);
});

// ─── Settings ─────────────────────────────────────────────────────────────────

export function getSetting(key) {
  const row = db.prepare('SELECT value FROM settings WHERE key = ?').get(key);
  return row ? JSON.parse(row.value) : null;
}

export function setSetting(key, value) {
  db.prepare(`
    INSERT INTO settings (key, value, updated_at)
    VALUES (?, ?, datetime('now'))
    ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = datetime('now')
  `).run(key, JSON.stringify(value));
}

export function getAllSettings() {
  const rows = db.prepare('SELECT key, value FROM settings').all();
  const out = {};
  for (const r of rows) {
    try {
      out[r.key] = JSON.parse(r.value);
    } catch {
      out[r.key] = r.value; // Go bot writes raw strings — fall back gracefully
    }
  }
  return out;
}

// ─── Sync log ────────────────────────────────────────────────────────────────

export function logSync({ trades_count, positions_count, status = 'ok', error_msg = null }) {
  db.prepare(`
    INSERT INTO sync_log (trades_count, positions_count, status, error_msg)
    VALUES (?, ?, ?, ?)
  `).run(trades_count, positions_count, status, error_msg);
}

export function getLastSync() {
  return db.prepare('SELECT * FROM sync_log ORDER BY id DESC LIMIT 1').get();
}

export default db;
