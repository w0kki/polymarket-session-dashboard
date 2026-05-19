/**
 * sync.js — Polymarket → SQLite sync module
 *
 * Fetches positions + activity from Polymarket, computes trade rows,
 * and upserts everything into the local SQLite database.
 *
 * Designed to run server-side (no DOM, no Vite proxy).
 * All Polymarket API calls go directly to data-api.polymarket.com.
 */

import { upsertTrades, upsertPositions, logSync } from './db.js';

const PROXY_WALLET = '0x8ab08a84e5a64bd46b62ad402a090971147350e3';
const API_BASE     = 'https://data-api.polymarket.com';
const HEADERS      = { 'User-Agent': 'Mozilla/5.0' };

// ─── API fetchers ─────────────────────────────────────────────────────────────

async function fetchPositions() {
  const url = `${API_BASE}/positions?user=${PROXY_WALLET}&sizeThreshold=.01&limit=100`;
  const res = await fetch(url, { headers: HEADERS });
  if (!res.ok) throw new Error(`Positions API ${res.status}`);
  return res.json();
}

async function fetchActivity() {
  const url = `${API_BASE}/activity?user=${PROXY_WALLET}&limit=500`;
  const res = await fetch(url, { headers: HEADERS });
  if (!res.ok) throw new Error(`Activity API ${res.status}`);
  return res.json();
}

// ─── Sport / fee helpers ──────────────────────────────────────────────────────

function detectSport(slug) {
  const prefix = slug.split('-')[0].toLowerCase();
  const map = {
    epl: 'Soccer', lal: 'Soccer', fl1: 'Soccer', fl2: 'Soccer',
    spl: 'Soccer', mls: 'Soccer', bun: 'Soccer', ser: 'Soccer',
    ucl: 'Soccer', uel: 'Soccer', wc:  'Soccer', afc: 'Soccer',
    mlb: 'Baseball', nba: 'Basketball', nfl: 'Football',
    nhl: 'Hockey',   atp: 'Tennis',     wta: 'Tennis', ufc: 'MMA',
  };
  return map[prefix] ?? 'Sports';
}

function feeRate(sport) {
  if (sport === 'Crypto')      return 0.072;
  if (['Finance', 'Politics', 'Tech'].includes(sport)) return 0.04;
  if (sport === 'Culture')     return 0.05;
  if (sport === 'Economics')   return 0.03;
  if (sport === 'Weather')     return 0.025;
  if (sport === 'Mentions')    return 0.25;
  if (sport === 'Geopolitics') return 0;
  return 0.03; // Sports default
}

function calcFee(shares, price, sport) {
  return shares * price * feeRate(sport) * price * (1 - price);
}

const SPORTS_CATS = new Set(['Soccer','Baseball','Basketball','Football','Hockey','Tennis','MMA']);

// ─── Trade builder (mirrors buildTradeLogRows in polymarket.ts) ───────────────

function buildTradeRows(positions, activity) {
  const buys = activity
    .filter(a => a.type === 'TRADE' && a.side === 'BUY')
    .sort((a, b) => a.timestamp - b.timestamp);

  const sells   = activity.filter(a => a.type === 'TRADE' && a.side === 'SELL');
  const redeems = activity.filter(a => a.type === 'REDEEM');

  const buyCost   = {};
  const buyShares = {};
  const buyPrice  = {};
  for (const b of buys) {
    buyCost[b.conditionId]   = (buyCost[b.conditionId]   ?? 0) + b.usdcSize;
    buyShares[b.conditionId] = (buyShares[b.conditionId] ?? 0) + b.size;
    buyPrice[b.conditionId]  = b.price;
  }

  const redeemedMap = {};
  for (const r of redeems) redeemedMap[r.conditionId] = r.usdcSize;

  const soldMap = {};
  for (const s of sells) soldMap[s.conditionId] = (soldMap[s.conditionId] ?? 0) + s.usdcSize;

  const posMap = {};
  for (const p of positions) posMap[p.conditionId] = p;

  const seen = new Set();
  const rows = [];

  for (const b of buys) {
    if (seen.has(b.conditionId)) continue;
    seen.add(b.conditionId);

    const cid       = b.conditionId;
    const shares    = buyShares[cid];
    const entry     = buyPrice[cid];
    const cost      = buyCost[cid];
    const size      = cost;
    const sport     = detectSport(b.slug);
    const tradeType = entry < 0.5 ? 'Latency Arb' : 'Risk Premia';
    const date      = new Date(b.timestamp * 1000).toISOString().split('T')[0];

    let exit    = null;
    let outcome = 'NA';
    let pnl     = null;

    if (redeemedMap[cid] !== undefined) {
      exit    = 1.00;
      outcome = 'WIN';
      pnl     = redeemedMap[cid] - cost;
    } else if (posMap[cid]) {
      const pos = posMap[cid];
      if (pos.curPrice >= 0.998) {
        exit    = 1.00;
        outcome = 'WIN';
        pnl     = pos.cashPnl;
      } else {
        exit    = null;
        outcome = 'NA';
        pnl     = null;
      }
    } else if (soldMap[cid] !== undefined) {
      const received = soldMap[cid];
      const sellAct  = sells.find(s => s.conditionId === cid);
      exit    = sellAct?.price ?? null;
      pnl     = received - cost;
      outcome = pnl >= 0 ? 'WIN' : 'LOSS';
    }

    const pnlPct    = pnl !== null && size > 0 ? pnl / size : null;
    const buyFee    = calcFee(shares, entry, sport);
    const sellFee   = (exit !== null && outcome !== 'WIN')
      ? calcFee(shares, exit, sport)
      : 0;
    const totalFees = buyFee + sellFee;
    const netPnl    = pnl !== null ? pnl - totalFees : null;
    const feeCat    = SPORTS_CATS.has(sport) ? 'Sports' : sport;

    rows.push({
      condition_id: cid,
      date,
      market:       b.title,
      slug:         b.slug,
      sport,
      trade_type:   tradeType,
      side:         b.outcome,
      entry_price:  entry,
      shares,
      size_usdc:    size,
      exit_price:   exit,
      outcome,
      pnl,
      pnl_pct:      pnlPct,
      fee_cat:      feeCat,
      buy_fee:      buyFee,
      sell_fee:     sellFee,
      total_fees:   totalFees,
      net_pnl:      netPnl,
      icon:         b.icon,
    });
  }

  return rows;
}

// ─── Position builder ─────────────────────────────────────────────────────────

function buildPositionRows(positions) {
  return positions.map(p => ({
    condition_id:  p.conditionId,
    title:         p.title,
    outcome:       p.outcome,
    size:          p.size,
    avg_price:     p.avgPrice,
    cur_price:     p.curPrice,
    initial_value: p.initialValue,
    current_value: p.currentValue,
    cash_pnl:      p.cashPnl,
    percent_pnl:   p.percentPnl,
    icon:          p.icon,
    slug:          p.slug,
    end_date:      p.endDate,
    redeemable:    p.redeemable ? 1 : 0,
  }));
}

// ─── Main sync ────────────────────────────────────────────────────────────────

export async function runSync() {
  const start = Date.now();
  console.log(`[sync] Starting at ${new Date().toISOString()}`);

  try {
    const [positions, activity] = await Promise.all([fetchPositions(), fetchActivity()]);

    const tradeRows    = buildTradeRows(positions, activity);
    const positionRows = buildPositionRows(positions);

    upsertTrades(tradeRows);
    upsertPositions(positionRows);

    logSync({
      trades_count:    tradeRows.length,
      positions_count: positionRows.length,
      status:          'ok',
    });

    const ms = Date.now() - start;
    console.log(`[sync] Done — ${tradeRows.length} trades, ${positionRows.length} positions (${ms}ms)`);

    return { trades: tradeRows.length, positions: positionRows.length };
  } catch (err) {
    const msg = err?.message ?? String(err);
    console.error(`[sync] Error: ${msg}`);
    logSync({ trades_count: 0, positions_count: 0, status: 'error', error_msg: msg });
    throw err;
  }
}
