import type { Position, Activity, SessionStats, TradeRow, TradeLogRow } from '../types';

// Proxy wallet address for w0kki1 — change this to switch accounts
export const PROXY_WALLET = '0x8ab08a84e5a64bd46b62ad402a090971147350e3';
const BASE = '/api/data';

export async function fetchPositions(): Promise<Position[]> {
  const res = await fetch(`${BASE}/positions?user=${PROXY_WALLET}&sizeThreshold=.01&limit=100`);
  if (!res.ok) throw new Error(`Positions API ${res.status}`);
  return res.json();
}

export async function fetchActivity(): Promise<Activity[]> {
  const res = await fetch(`${BASE}/activity?user=${PROXY_WALLET}&limit=500`);
  if (!res.ok) throw new Error(`Activity API ${res.status}`);
  return res.json();
}

export function computeStats(positions: Position[], activity: Activity[]): SessionStats {
  const buys   = activity.filter(a => a.type === 'TRADE' && a.side === 'BUY');
  const sells  = activity.filter(a => a.type === 'TRADE' && a.side === 'SELL');
  const redeems = activity.filter(a => a.type === 'REDEEM');

  // Cost basis per conditionId
  const buyCost: Record<string, number> = {};
  for (const b of buys) buyCost[b.conditionId] = (buyCost[b.conditionId] ?? 0) + b.usdcSize;

  // Realised P&L from winning redemptions
  const redeemedPnl = redeems.reduce(
    (sum, r) => sum + r.usdcSize - (buyCost[r.conditionId] ?? 0), 0
  );

  // Realised P&L from early sells
  const soldMap: Record<string, number> = {};
  for (const s of sells) soldMap[s.conditionId] = (soldMap[s.conditionId] ?? 0) + s.usdcSize;
  const sellPnl = Object.entries(soldMap).reduce(
    (sum, [cid, received]) => sum + received - (buyCost[cid] ?? 0), 0
  );

  const unrealizedPnl  = positions.reduce((sum, p) => sum + p.cashPnl, 0);
  const portfolioValue = positions.reduce((sum, p) => sum + p.currentValue, 0);

  // Win counting: definitive (REDEEM) + probable (curPrice ≥ 0.998)
  const redeemedIds = new Set(redeems.map(r => r.conditionId));
  const resolvedWins  = redeems.length;
  const probableWins  = positions.filter(p => p.curPrice >= 0.998 && !redeemedIds.has(p.conditionId)).length;
  const totalWins     = resolvedWins + probableWins;
  const totalLosses   = Object.entries(soldMap).filter(
    ([cid, received]) => received < (buyCost[cid] ?? 0)
  ).length;

  const totalRealizedPnl = redeemedPnl + sellPnl;
  const totalPnl = totalRealizedPnl + unrealizedPnl;

  // Fees: Sports fee = shares × price × 0.03 × price × (1 − price)
  const totalFees = buys.reduce(
    (sum, b) => sum + b.size * b.price * 0.03 * b.price * (1 - b.price), 0
  );

  const allPnls = [
    ...redeems.map(r => r.usdcSize - (buyCost[r.conditionId] ?? 0)),
    ...positions.map(p => p.cashPnl),
  ];
  const largestWin  = allPnls.length ? Math.max(...allPnls) : 0;
  const largestLoss = allPnls.length ? Math.min(...allPnls) : 0;

  const n = buys.length;
  return {
    totalTrades: n,
    portfolioValue,
    unrealizedPnl,
    totalRealizedPnl,
    totalPnl,
    winRate: (totalWins + totalLosses) > 0 ? totalWins / (totalWins + totalLosses) : 0,
    wins: totalWins,
    losses: totalLosses,
    activePositions: positions.filter(p => p.curPrice < 0.998).length,
    largestWin,
    largestLoss: largestLoss < 0 ? largestLoss : 0,
    avgReturn: n > 0 ? totalPnl / n : 0,
    totalFees,
    netPnl: totalPnl - totalFees,
    avgFeePerTrade: n > 0 ? totalFees / n : 0,
  };
}

function detectSport(slug: string): string {
  const prefix = slug.split('-')[0].toLowerCase();
  const map: Record<string, string> = {
    epl: 'Soccer', lal: 'Soccer', fl1: 'Soccer', fl2: 'Soccer',
    spl: 'Soccer', mls: 'Soccer', bun: 'Soccer', ser: 'Soccer',
    ucl: 'Soccer', uel: 'Soccer', wc: 'Soccer', afc: 'Soccer',
    mlb: 'Baseball', nba: 'Basketball', nfl: 'Football',
    nhl: 'Hockey', atp: 'Tennis', wta: 'Tennis', ufc: 'MMA',
  };
  return map[prefix] ?? 'Sports';
}

function feeRate(sport: string): number {
  if (sport === 'Crypto') return 0.072;
  if (['Finance', 'Politics', 'Tech'].includes(sport)) return 0.04;
  if (sport === 'Culture') return 0.05;
  if (sport === 'Economics') return 0.03;
  if (sport === 'Weather') return 0.025;
  if (sport === 'Mentions') return 0.25;
  if (sport === 'Geopolitics') return 0;
  return 0.03; // Sports default
}

function calcFee(shares: number, price: number, sport: string): number {
  return shares * price * feeRate(sport) * price * (1 - price);
}

export function buildTradeLogRows(positions: Position[], activity: Activity[]): TradeLogRow[] {
  const buys = activity
    .filter(a => a.type === 'TRADE' && a.side === 'BUY')
    .sort((a, b) => a.timestamp - b.timestamp);

  const sells   = activity.filter(a => a.type === 'TRADE' && a.side === 'SELL');
  const redeems = activity.filter(a => a.type === 'REDEEM');

  const buyCost: Record<string, number> = {};
  const buyShares: Record<string, number> = {};
  const buyPrice: Record<string, number> = {};
  for (const b of buys) {
    buyCost[b.conditionId]   = (buyCost[b.conditionId]   ?? 0) + b.usdcSize;
    buyShares[b.conditionId] = (buyShares[b.conditionId] ?? 0) + b.size;
    buyPrice[b.conditionId]  = b.price; // last (or only) buy price
  }

  const redeemedMap: Record<string, number> = {};
  for (const r of redeems) redeemedMap[r.conditionId] = r.usdcSize;

  const soldMap: Record<string, number> = {};
  for (const s of sells) soldMap[s.conditionId] = (soldMap[s.conditionId] ?? 0) + s.usdcSize;

  const posMap: Record<string, Position> = {};
  for (const p of positions) posMap[p.conditionId] = p;

  const seen = new Set<string>();
  const rows: TradeLogRow[] = [];

  for (const b of buys) {
    if (seen.has(b.conditionId)) continue;
    seen.add(b.conditionId);

    const cid     = b.conditionId;
    const shares  = buyShares[cid];
    const entry   = buyPrice[cid];
    const cost    = buyCost[cid];
    const size    = cost;                             // I = G × H (same as usdcSize)
    const sport   = detectSport(b.slug);
    const tradeType = entry < 0.5 ? 'Latency Arb' : 'Risk Premia';
    const date    = new Date(b.timestamp * 1000).toISOString().split('T')[0];

    let exit: number | null = null;
    let outcome = 'NA';
    let pnl: number | null = null;

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

    const pnlPct   = pnl !== null && size > 0 ? pnl / size : null;
    const buyFee   = calcFee(shares, entry, sport);
    const sellFee  = (exit !== null && outcome !== 'WIN')
      ? calcFee(shares, exit, sport)
      : 0;
    const totalFees = buyFee + sellFee;
    const netPnl    = pnl !== null ? pnl - totalFees : null;

    rows.push({
      num: rows.length + 1,
      date,
      market: b.title,
      sport,
      type: tradeType,
      side: b.outcome,
      entry,
      shares,
      size,
      exit,
      outcome,
      pnl,
      pnlPct,
      notes: '',
      feeCat: sport === 'Soccer' || sport === 'Baseball' || sport === 'Basketball'
           || sport === 'Football' || sport === 'Hockey' || sport === 'Tennis'
           || sport === 'MMA' ? 'Sports' : sport,
      buyFee,
      sellFee,
      totalFees,
      netPnl,
      icon: b.icon,
    });
  }

  return rows;
}

export function buildTradeRows(positions: Position[], activity: Activity[]): TradeRow[] {
  const buys = activity
    .filter(a => a.type === 'TRADE' && a.side === 'BUY')
    .sort((a, b) => a.timestamp - b.timestamp);

  const sells  = activity.filter(a => a.type === 'TRADE' && a.side === 'SELL');
  const redeems = activity.filter(a => a.type === 'REDEEM');

  const buyCost: Record<string, number> = {};
  for (const b of buys) buyCost[b.conditionId] = (buyCost[b.conditionId] ?? 0) + b.usdcSize;

  const redeemedMap: Record<string, number> = {};
  for (const r of redeems) redeemedMap[r.conditionId] = r.usdcSize;

  const soldMap: Record<string, number> = {};
  for (const s of sells) soldMap[s.conditionId] = (soldMap[s.conditionId] ?? 0) + s.usdcSize;

  const posMap: Record<string, Position> = {};
  for (const p of positions) posMap[p.conditionId] = p;

  // One row per unique conditionId (in order of first buy)
  const seen = new Set<string>();
  let cumulative = 0;
  const rows: TradeRow[] = [];

  for (const b of buys) {
    if (seen.has(b.conditionId)) continue;
    seen.add(b.conditionId);

    const cost = buyCost[b.conditionId] ?? 0;
    let pnl: number;
    let status: TradeRow['status'];

    if (redeemedMap[b.conditionId] !== undefined) {
      pnl = redeemedMap[b.conditionId] - cost;
      status = 'WIN';
    } else if (posMap[b.conditionId]) {
      const pos = posMap[b.conditionId];
      pnl = pos.cashPnl;
      status = pos.curPrice >= 0.998 ? 'WIN' : 'ACTIVE';
    } else if (soldMap[b.conditionId] !== undefined) {
      pnl = soldMap[b.conditionId] - cost;
      status = pnl >= 0 ? 'WIN' : 'SOLD';
    } else {
      pnl = 0;
      status = 'ACTIVE';
    }

    cumulative += pnl;
    rows.push({
      index: rows.length + 1,
      title: b.title,
      outcome: b.outcome,
      size: b.size,
      price: b.price,
      cost,
      pnl,
      cumulative,
      status,
      timestamp: b.timestamp,
      icon: b.icon,
    });
  }

  return rows;
}
