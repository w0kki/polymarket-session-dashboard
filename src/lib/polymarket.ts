import type { Position, Activity, SessionStats, TradeRow } from '../types';

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
