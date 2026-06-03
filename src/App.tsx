import { useState, useEffect, useCallback, useRef } from 'react';
import type { Position, SessionStats, TradeRow, TradeLogRow } from './types';
import { fetchPositions, fetchActivity, computeStats, buildTradeRows, buildTradeLogRows, PROXY_WALLET } from './lib/polymarket';

const REFRESH_INTERVAL = 60 * 60 * 1000;

async function fetchPaperTrades(): Promise<TradeLogRow[]> {
  try {
    const res = await fetch('/api/trades?limit=500');
    if (!res.ok) return [];
    const rows: any[] = await res.json();
    return rows
      .filter(r => r.trade_type === 'Paper')
      .map(r => ({
        num: 0,
        date: r.date,
        // first_seen_at is UTC "YYYY-MM-DD HH:MM:SS"; fall back to date if missing.
        ts: r.first_seen_at
          ? Date.parse(r.first_seen_at.replace(' ', 'T') + 'Z') / 1000
          : (r.date ? Date.parse(r.date + 'T00:00:00Z') / 1000 : 0),
        market: r.market,
        sport: r.sport,
        type: r.trade_type,
        side: r.side,
        entry: r.entry_price,
        shares: r.shares,
        size: r.size_usdc,
        exit: r.exit_price ?? null,
        outcome: r.outcome,
        pnl: r.pnl ?? null,
        pnlPct: r.pnl_pct ?? null,
        notes: '',
        feeCat: r.fee_cat ?? '',
        buyFee: r.buy_fee ?? 0,
        sellFee: r.sell_fee ?? 0,
        totalFees: r.total_fees ?? 0,
        netPnl: r.net_pnl ?? null,
        icon: r.icon ?? '',
        slug: r.slug ?? '',
        conditionId: r.condition_id ?? '',
      }));
  } catch {
    return [];
  }
}

// Fetch live token prices for open paper positions via the CLOB proxy.
// Returns a map of `${conditionId}:${outcome}` → current price.
async function fetchLivePrices(trades: TradeLogRow[]): Promise<Record<string, number>> {
  const open = trades.filter(t => t.outcome === 'NA' && t.conditionId);
  if (open.length === 0) return {};

  const results = await Promise.all(
    open.map(async (t) => {
      try {
        const res = await fetch(`/api/clob/markets/${t.conditionId}`);
        if (!res.ok) return null;
        const data = await res.json();
        const token = (data.tokens ?? []).find((tk: any) => tk.outcome === t.side);
        if (!token) return null;
        return { key: `${t.conditionId}:${t.side}`, price: token.price as number };
      } catch {
        return null;
      }
    })
  );

  const priceMap: Record<string, number> = {};
  for (const r of results) {
    if (r !== null) priceMap[r.key] = r.price;
  }
  return priceMap;
}

const fmt$ = (n: number, decimals = 2) => {
  const abs = Math.abs(n).toLocaleString('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: decimals, maximumFractionDigits: decimals });
  return (n >= 0 ? '+' : '-') + abs;
};
const fmt$abs = (n: number) =>
  n.toLocaleString('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2 });
const fmtPct = (n: number) => (n * 100).toFixed(1) + '%';
const fmtCents = (n: number) => Math.round(n * 100) + '¢';

function StatusBadge({ status }: { status: TradeRow['status'] }) {
  const styles: Record<TradeRow['status'], string> = {
    WIN:    'bg-emerald-500/15 text-emerald-400 border border-emerald-500/30',
    LOSS:   'bg-red-500/15 text-red-400 border border-red-500/30',
    ACTIVE: 'bg-blue-500/15 text-blue-400 border border-blue-500/30',
    SOLD:   'bg-yellow-500/15 text-yellow-400 border border-yellow-500/30',
  };
  return (
    <span className={`text-xs font-medium px-2 py-0.5 rounded-full ${styles[status]}`}>
      {status}
    </span>
  );
}

function KpiCard({ label, value, sub, color = 'default' }: {
  label: string; value: string; sub?: string; color?: 'green' | 'red' | 'blue' | 'default';
}) {
  const valueColor = {
    green: 'text-emerald-400',
    red: 'text-red-400',
    blue: 'text-blue-400',
    default: 'text-white',
  }[color];
  return (
    <div className="bg-gray-900 border border-gray-800 rounded-xl p-4 space-y-1">
      <div className="text-xs text-gray-500 uppercase tracking-wider">{label}</div>
      <div className={`text-xl font-bold tabular-nums ${valueColor}`}>{value}</div>
      {sub && <div className="text-xs text-gray-600">{sub}</div>}
    </div>
  );
}

function KellyView({ stats, tradeLog }: { stats: SessionStats; tradeLog: TradeLogRow[] }) {
  const [bankroll,    setBankrollRaw]  = useState(500);
  const [winProb,     setWinProbRaw]   = useState(0.90);
  const [entryPrice,  setEntryRaw]     = useState(0.85);
  const saveTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Load settings from server on mount
  useEffect(() => {
    fetch('/api/settings').then(r => r.json()).then(d => {
      if (d.bankroll)    setBankrollRaw(d.bankroll);
      if (d.winProb)     setWinProbRaw(d.winProb);
      if (d.entryPrice)  setEntryRaw(d.entryPrice);
    }).catch(() => {});
  }, []);

  const persist = (b: number, w: number, e: number) => {
    if (saveTimer.current) clearTimeout(saveTimer.current);
    saveTimer.current = setTimeout(() => {
      fetch('/api/settings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ bankroll: b, winProb: w, entryPrice: e }),
      }).catch(() => {});
    }, 600);
  };

  const setBankroll   = (v: number) => { setBankrollRaw(v);  persist(v, winProb, entryPrice); };
  const setWinProb    = (v: number) => { setWinProbRaw(v);   persist(bankroll, v, entryPrice); };
  const setEntryPrice = (v: number) => { setEntryRaw(v);     persist(bankroll, winProb, v); };

  // Historical stats from trade log
  const winPnls = tradeLog.filter(r => r.outcome === 'WIN' && r.pnl !== null).map(r => r.pnl!);
  const lossPnls = tradeLog.filter(r => (r.outcome === 'LOSS' || r.outcome === 'STOP_LOSS') && r.pnl !== null).map(r => Math.abs(r.pnl!));
  const avgWin  = winPnls.length  > 0 ? winPnls.reduce((a, b)  => a + b, 0) / winPnls.length  : null;
  const avgLoss = lossPnls.length > 0 ? lossPnls.reduce((a, b) => a + b, 0) / lossPnls.length : null;
  const histTotal = stats.wins + stats.losses;
  const histP = histTotal > 0 ? stats.wins / histTotal : null;
  const histQ = histP !== null ? 1 - histP : null;
  const histB = avgWin !== null && avgLoss !== null && avgLoss > 0 ? avgWin / avgLoss : null;
  const histFull    = histB !== null && histP !== null && histQ !== null ? Math.max(0, (histB * histP - histQ) / histB) : null;
  const histHalf    = histFull !== null ? histFull / 2 : null;
  const histQuarter = histFull !== null ? histFull / 4 : null;

  // Manual scenario
  const winPerShare  = 1 - entryPrice;
  const lossPerShare = entryPrice;
  const bManual      = lossPerShare > 0 ? winPerShare / lossPerShare : 0;
  const qManual      = 1 - winProb;
  const fullKelly    = Math.max(0, (bManual * winProb - qManual) / bManual);
  const halfKelly    = fullKelly / 2;
  const quarterKelly = fullKelly / 4;
  const positionSize = halfKelly * bankroll;

  // Geometric growth rate table (5% increments)
  const growthRows = Array.from({ length: 19 }, (_, i) => {
    const f = parseFloat(((i + 1) * 0.05).toFixed(2));
    const g = f < 1 ? winProb * Math.log(1 + bManual * f) + qManual * Math.log(1 - f) : null;
    return { f, g };
  });
  const maxG = Math.max(...growthRows.map(r => r.g ?? -Infinity));

  const statRow = (label: string, value: string | null, color?: string) => (
    <div className="flex justify-between items-center py-2 border-b border-gray-800/60">
      <span className="text-xs text-gray-400">{label}</span>
      <span className={`text-sm font-medium tabular-nums ${color ?? 'text-gray-200'}`}>{value ?? '—'}</span>
    </div>
  );

  const inputRow = (label: string, value: number, onChange: (v: number) => void, step = 0.01, min = 0, max = 1) => (
    <div className="flex justify-between items-center py-2 border-b border-gray-800/60">
      <span className="text-xs text-gray-400">{label}</span>
      <input
        type="number" value={value} step={step} min={min} max={max}
        onChange={e => onChange(parseFloat(e.target.value) || 0)}
        className="w-24 bg-gray-800 border border-gray-700 rounded px-2 py-1 text-sm text-right tabular-nums text-white focus:outline-none focus:border-blue-500"
      />
    </div>
  );

  return (
    <div className="animate-fade-in space-y-6">
      <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider">
        Kelly Criterion Calculator
      </h2>

      {/* Top two-column layout */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">

        {/* Left — Historical Kelly */}
        <div className="bg-gray-900 border border-gray-800 rounded-xl p-5 space-y-1">
          <div className="text-xs font-semibold text-gray-500 uppercase tracking-wider mb-3">Your Trade Data</div>
          {statRow('Total Trades', String(histTotal))}
          {statRow('Win Probability (p)', histP !== null ? fmtPct(histP) : null, 'text-emerald-400')}
          {statRow('Loss Probability (q)', histQ !== null ? fmtPct(histQ) : null, 'text-red-400')}
          {statRow('Average Win', avgWin !== null ? fmt$abs(avgWin) : null, 'text-emerald-400')}
          {statRow('Average Loss', avgLoss !== null ? fmt$abs(avgLoss) : null, 'text-red-400')}
          {statRow('b (Win/Loss ratio)', histB !== null ? histB.toFixed(3) : null)}

          <div className="text-xs font-semibold text-gray-500 uppercase tracking-wider mt-4 mb-2 pt-2">Kelly Sizing</div>
          {statRow('Full Kelly (f*)', histFull !== null ? fmtPct(histFull) : null)}
          {statRow('Half-Kelly (recommended)', histHalf !== null ? fmtPct(histHalf) : null, 'text-blue-400')}
          {statRow('Quarter-Kelly (conservative)', histQuarter !== null ? fmtPct(histQuarter) : null, 'text-gray-300')}
          {histFull === null && (
            <p className="text-xs text-gray-600 mt-2 italic">Kelly sizing requires at least one loss to compute b.</p>
          )}
        </div>

        {/* Right — Manual Scenario */}
        <div className="bg-gray-900 border border-gray-800 rounded-xl p-5 space-y-1">
          <div className="text-xs font-semibold text-gray-500 uppercase tracking-wider mb-3">Manual Scenario Calculator</div>
          {inputRow('Bankroll (USDC)', bankroll, setBankroll, 10, 1, 100000)}
          {inputRow('Win Probability', winProb, setWinProb, 0.01, 0.01, 0.99)}
          {inputRow('Entry Price (¢)', entryPrice, setEntryPrice, 0.01, 0.01, 0.99)}

          <div className="text-xs font-semibold text-gray-500 uppercase tracking-wider mt-4 mb-2 pt-2">Results</div>
          {statRow('Win per Share', winPerShare.toFixed(4))}
          {statRow('Loss per Share', lossPerShare.toFixed(4))}
          {statRow('b (Win/Loss)', bManual.toFixed(4))}
          {statRow('Full Kelly f*', fmtPct(fullKelly))}
          {statRow('Half-Kelly f*', fmtPct(halfKelly), 'text-blue-400')}
          {statRow('Quarter-Kelly f*', fmtPct(quarterKelly), 'text-gray-300')}
          {statRow('Half-Kelly Position', fmt$abs(positionSize), positionSize > 0 ? 'text-emerald-400' : 'text-gray-400')}
        </div>
      </div>

      {/* Growth Rate Table */}
      <div className="bg-gray-900 border border-gray-800 rounded-xl p-5">
        <div className="text-xs font-semibold text-gray-500 uppercase tracking-wider mb-3">
          Geometric Growth Rate by Bet Size
          <span className="normal-case font-normal text-gray-600 ml-2">(based on manual scenario)</span>
        </div>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-gray-500 text-xs uppercase tracking-wider border-b border-gray-800">
                <th className="text-left py-2 pr-4">Bet Size (% Bankroll)</th>
                <th className="text-right py-2 pr-4">Position (USDC)</th>
                <th className="text-right py-2">Geometric Growth Rate</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800/40">
              {growthRows.map(({ f, g }) => {
                const isOptimal = g !== null && Math.abs(g - maxG) < 0.0000001 && maxG > 0;
                const isHalf = Math.abs(f - halfKelly) < 0.025;
                return (
                  <tr key={f} className={`${isOptimal ? 'bg-emerald-500/10' : isHalf ? 'bg-blue-500/5' : ''}`}>
                    <td className="py-1.5 pr-4 tabular-nums text-gray-300">
                      {(f * 100).toFixed(0)}%
                      {isOptimal && <span className="ml-2 text-xs text-emerald-400">← optimal</span>}
                      {isHalf && !isOptimal && <span className="ml-2 text-xs text-blue-400">← ½-Kelly</span>}
                    </td>
                    <td className="py-1.5 pr-4 text-right tabular-nums text-gray-400">{fmt$abs(f * bankroll)}</td>
                    <td className={`py-1.5 text-right tabular-nums font-medium ${g === null ? 'text-gray-600' : g >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                      {g === null ? '—' : (g * 100).toFixed(4) + '%'}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}

function TradeLogView({ tradeLog }: { tradeLog: TradeLogRow[] }) {
  const [filter, setFilter] = useState<'all' | 'live' | 'paper'>('all');
  const [sortDir, setSortDir] = useState<'desc' | 'asc'>('desc'); // newest first by default

  const filtered = filter === 'live'
    ? tradeLog.filter(r => r.type !== 'Paper')
    : filter === 'paper'
      ? tradeLog.filter(r => r.type === 'Paper')
      : tradeLog;

  // Sort by actual order time; each row keeps its chronological #.
  const visible = [...filtered].sort((a, b) =>
    sortDir === 'asc' ? a.ts - b.ts : b.ts - a.ts
  );

  return (
    <section className="animate-fade-in">
      <div className="flex items-center gap-3 mb-3">
        <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider">
          Trade Log ({visible.length} trades)
        </h2>
        <div className="flex gap-1">
          <button
            onClick={() => setFilter(f => f === 'live' ? 'all' : 'live')}
            className={`text-xs px-2.5 py-0.5 rounded-full border transition-colors ${
              filter === 'live'
                ? 'bg-orange-500/20 text-orange-400 border-orange-500/40'
                : 'bg-gray-800 text-gray-500 border-gray-700 hover:text-gray-300 hover:border-gray-600'
            }`}
          >Live</button>
          <button
            onClick={() => setFilter(f => f === 'paper' ? 'all' : 'paper')}
            className={`text-xs px-2.5 py-0.5 rounded-full border transition-colors ${
              filter === 'paper'
                ? 'bg-cyan-500/20 text-cyan-400 border-cyan-500/40'
                : 'bg-gray-800 text-gray-500 border-gray-700 hover:text-gray-300 hover:border-gray-600'
            }`}
          >Paper</button>
          <button
            onClick={() => setSortDir(d => d === 'desc' ? 'asc' : 'desc')}
            title="Toggle sort by order time"
            className="text-xs px-2.5 py-0.5 rounded-full border transition-colors bg-gray-800 text-gray-400 border-gray-700 hover:text-gray-200 hover:border-gray-600"
          >{sortDir === 'desc' ? 'Newest first ↓' : 'Oldest first ↑'}</button>
        </div>
      </div>
      <div className="rounded-xl border border-gray-800 overflow-x-auto">
        <table className="w-full text-sm whitespace-nowrap">
          <thead>
            <tr className="bg-gray-900 text-gray-500 text-xs uppercase tracking-wider">
              <th className="text-right px-3 py-3">#</th>
              <th className="text-left px-3 py-3">Date</th>
              <th className="text-left px-3 py-3 min-w-48">Market</th>
              <th className="text-left px-3 py-3">Sport</th>
              <th className="text-left px-3 py-3">Type</th>
              <th className="text-left px-3 py-3">Side</th>
              <th className="text-right px-3 py-3">Entry</th>
              <th className="text-right px-3 py-3">Shares</th>
              <th className="text-right px-3 py-3">Size</th>
              <th className="text-right px-3 py-3">Exit</th>
              <th className="text-center px-3 py-3">Outcome</th>
              <th className="text-right px-3 py-3">P&amp;L</th>
              <th className="text-right px-3 py-3">P&amp;L%</th>
              <th className="text-left px-3 py-3">Fee Cat</th>
              <th className="text-right px-3 py-3">Buy Fee</th>
              <th className="text-right px-3 py-3">Sell Fee</th>
              <th className="text-right px-3 py-3">Total Fee</th>
              <th className="text-right px-3 py-3">Net P&amp;L</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-gray-800/50">
            {visible.map((row) => (
              <tr key={row.num} className="bg-gray-950 hover:bg-gray-900/50 transition-colors">
                <td className="px-3 py-2.5 text-right text-gray-600 tabular-nums">{row.num}</td>
                <td className="px-3 py-2.5 text-gray-400 tabular-nums">{row.date}</td>
                <td className="px-3 py-2.5">
                  <div className="flex items-center gap-2">
                    {row.icon && (
                      <img src={row.icon} alt="" className="w-4 h-4 rounded-full object-cover shrink-0" />
                    )}
                    {row.slug ? (
                      <a
                        href={`https://polymarket.com/event/${row.slug}`}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="text-gray-200 line-clamp-1 max-w-48 hover:text-blue-400 hover:underline transition-colors"
                      >{row.market}</a>
                    ) : (
                      <span className="text-gray-200 line-clamp-1 max-w-48">{row.market}</span>
                    )}
                  </div>
                </td>
                <td className="px-3 py-2.5 text-gray-400">{row.sport}</td>
                <td className="px-3 py-2.5">
                  <span className={`text-xs px-1.5 py-0.5 rounded ${
                    row.type === 'Latency Arb' ? 'text-purple-400 bg-purple-500/10' :
                    row.type === 'Paper'        ? 'text-cyan-400 bg-cyan-500/10' :
                                                  'text-orange-400 bg-orange-500/10'
                  }`}>
                    {row.type}
                  </span>
                </td>
                <td className="px-3 py-2.5 text-blue-300">{row.side}</td>
                <td className="px-3 py-2.5 text-right tabular-nums text-gray-300">{fmtCents(row.entry)}</td>
                <td className="px-3 py-2.5 text-right tabular-nums text-gray-300">{row.shares.toFixed(1)}</td>
                <td className="px-3 py-2.5 text-right tabular-nums text-gray-400">{fmt$abs(row.size)}</td>
                <td className="px-3 py-2.5 text-right tabular-nums text-gray-300">
                  {row.exit !== null ? fmtCents(row.exit) : '—'}
                </td>
                <td className="px-3 py-2.5 text-center">
                  {row.outcome === 'WIN' ? (
                    <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-emerald-500/15 text-emerald-400 border border-emerald-500/30">WIN</span>
                  ) : row.outcome === 'LOSS' ? (
                    <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-red-500/15 text-red-400 border border-red-500/30">LOSS</span>
                  ) : row.outcome === 'STOP_LOSS' ? (
                    <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-orange-500/15 text-orange-400 border border-orange-500/30">STOP</span>
                  ) : (
                    <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-gray-500/15 text-gray-500 border border-gray-500/30">—</span>
                  )}
                </td>
                <td className={`px-3 py-2.5 text-right tabular-nums font-medium ${row.pnl === null ? 'text-gray-600' : row.pnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                  {row.pnl === null ? '—' : fmt$(row.pnl)}
                </td>
                <td className={`px-3 py-2.5 text-right tabular-nums ${row.pnlPct === null ? 'text-gray-600' : row.pnlPct >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                  {row.pnlPct === null ? '—' : fmtPct(row.pnlPct)}
                </td>
                <td className="px-3 py-2.5 text-gray-500 text-xs">{row.feeCat}</td>
                <td className="px-3 py-2.5 text-right tabular-nums text-gray-500 text-xs">{fmt$abs(row.buyFee)}</td>
                <td className="px-3 py-2.5 text-right tabular-nums text-gray-500 text-xs">{fmt$abs(row.sellFee)}</td>
                <td className="px-3 py-2.5 text-right tabular-nums text-gray-500 text-xs">{fmt$abs(row.totalFees)}</td>
                <td className={`px-3 py-2.5 text-right tabular-nums font-medium text-xs ${row.netPnl === null ? 'text-gray-600' : row.netPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                  {row.netPnl === null ? '—' : fmt$(row.netPnl)}
                </td>
              </tr>
            ))}
          </tbody>
          <tfoot>
            {(() => {
              const rowPnl      = tradeLog.reduce((s, r) => s + (r.pnl      ?? 0), 0);
              const rowBuyFee   = tradeLog.reduce((s, r) => s + r.buyFee,           0);
              const rowSellFee  = tradeLog.reduce((s, r) => s + r.sellFee,          0);
              const rowTotalFee = tradeLog.reduce((s, r) => s + r.totalFees,        0);
              const rowNetPnl   = tradeLog.reduce((s, r) => s + (r.netPnl   ?? 0), 0);
              return (
                <tr className="bg-gray-900 border-t border-gray-700">
                  {/* cols 1–11: # … Outcome */}
                  <td colSpan={11} className="px-3 py-3 text-xs text-gray-500 uppercase tracking-wider">Totals</td>
                  {/* col 12: P&L */}
                  <td className={`px-3 py-3 text-right tabular-nums font-bold ${rowPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    {fmt$(rowPnl)}
                  </td>
                  {/* col 13: P&L% — blank */}
                  <td />
                  {/* col 14: Fee Cat — blank */}
                  <td />
                  {/* col 15: Buy Fee */}
                  <td className="px-3 py-3 text-right tabular-nums font-bold text-gray-400 text-xs">
                    {fmt$abs(rowBuyFee)}
                  </td>
                  {/* col 16: Sell Fee */}
                  <td className="px-3 py-3 text-right tabular-nums font-bold text-gray-400 text-xs">
                    {fmt$abs(rowSellFee)}
                  </td>
                  {/* col 17: Total Fee */}
                  <td className="px-3 py-3 text-right tabular-nums font-bold text-gray-400 text-xs">
                    {fmt$abs(rowTotalFee)}
                  </td>
                  {/* col 18: Net P&L */}
                  <td className={`px-3 py-3 text-right tabular-nums font-bold text-xs ${rowNetPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    {fmt$(rowNetPnl)}
                  </td>
                </tr>
              );
            })()}
          </tfoot>
        </table>
      </div>
    </section>
  );
}

// currentWinStreak counts consecutive WINs ending at the most recent RESOLVED
// trade. `outcomes` must be in chronological order (oldest → newest). Open trades
// (ACTIVE/NA) are skipped (not yet resolved); any loss/stop/sale ends the streak.
function currentWinStreak(outcomes: string[]): number {
  let streak = 0;
  for (let i = outcomes.length - 1; i >= 0; i--) {
    const o = outcomes[i];
    if (o === 'ACTIVE' || o === 'NA') continue; // not resolved yet — skip
    if (o === 'WIN') { streak++; continue; }
    break; // LOSS / STOP_LOSS / SOLD ends the streak
  }
  return streak;
}

function PaperDashboardView({ paperTrades }: { paperTrades: TradeLogRow[] }) {
  const resolved  = paperTrades.filter(r => r.outcome === 'WIN' || r.outcome === 'LOSS' || r.outcome === 'STOP_LOSS');
  const open      = paperTrades.filter(r => r.outcome === 'NA');
  const wins      = resolved.filter(r => r.outcome === 'WIN').length;
  const losses    = resolved.filter(r => r.outcome === 'LOSS' || r.outcome === 'STOP_LOSS').length;
  const winRate   = resolved.length > 0 ? wins / resolved.length : 0;
  const totalPnl  = resolved.reduce((s, r) => s + (r.netPnl ?? r.pnl ?? 0), 0);
  const totalFees = resolved.reduce((s, r) => s + r.totalFees, 0);
  const netPnl    = totalPnl;
  const portfolioValue = open.reduce((s, r) => s + r.size, 0);
  const largestWin  = resolved.filter(r => r.outcome === 'WIN').reduce((m, r) => Math.max(m, r.netPnl ?? r.pnl ?? 0), 0);
  const largestLoss = resolved.filter(r => r.outcome === 'LOSS' || r.outcome === 'STOP_LOSS').reduce((m, r) => Math.min(m, r.netPnl ?? r.pnl ?? 0), 0);
  const avgReturn   = resolved.length > 0 ? totalPnl / resolved.length : 0;
  const avgFee      = paperTrades.length > 0 ? totalFees / paperTrades.length : 0;
  const winStreak   = currentWinStreak([...resolved].sort((a, b) => (a.ts ?? 0) - (b.ts ?? 0)).map(r => r.outcome));

  let cumulative = 0;
  const runningRows: TradeRow[] = resolved.map((r, i) => {
    const tradePnl = r.netPnl ?? r.pnl ?? 0;
    cumulative += tradePnl;
    return {
      index: i + 1,
      title: r.market,
      outcome: r.side,
      size: r.shares,
      price: r.entry,
      cost: r.size,
      pnl: tradePnl,
      cumulative,
      status: r.outcome === 'WIN' ? 'WIN' : 'LOSS',
      timestamp: 0,
      icon: r.icon,
      slug: r.slug ?? '',
    };
  });

  return (
    <>
      {/* KPI Row 1 */}
      <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3 animate-fade-in">
        <KpiCard label="Paper Trades"    value={String(paperTrades.length)} />
        <KpiCard label="Win Rate"        value={resolved.length > 0 ? fmtPct(winRate) : '—'} color="green" />
        <KpiCard label="Portfolio Value" value={fmt$abs(portfolioValue)} sub="open positions" />
        <KpiCard
          label="Total P&L"
          value={resolved.length > 0 ? fmt$(totalPnl) : '—'}
          sub={`${fmt$(totalPnl, 2)} realized`}
          color={totalPnl >= 0 ? 'green' : 'red'}
        />
        <KpiCard label="Largest Win"  value={largestWin  > 0 ? fmt$(largestWin)  : '—'} color="green" />
        <KpiCard label="Largest Loss" value={largestLoss < 0 ? fmt$(largestLoss) : '—'} color={largestLoss < 0 ? 'red' : 'default'} />
      </div>

      {/* KPI Row 2 */}
      <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3 animate-fade-in">
        <KpiCard label="Wins"               value={String(wins)}   color="green" />
        <KpiCard label="Losses"             value={String(losses)} color={losses > 0 ? 'red' : 'default'} />
        <KpiCard label="Win Streak"         value={String(winStreak)} sub="current" color={winStreak > 0 ? 'green' : 'default'} />
        <KpiCard label="Total Fees"         value={fmt$abs(totalFees)} sub={`${fmt$abs(avgFee)} avg/trade`} />
        <KpiCard label="Net P&L (after fees)" value={resolved.length > 0 ? fmt$(netPnl) : '—'} color={netPnl >= 0 ? 'green' : 'red'} />
        <KpiCard label="Avg Return/Trade"   value={resolved.length > 0 ? fmt$(avgReturn) : '—'} color={avgReturn >= 0 ? 'green' : 'red'} />
      </div>

      {/* Open Paper Positions */}
      <section className="animate-fade-in">
        <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider mb-3">
          Open Paper Positions ({open.length})
        </h2>
        <div className="rounded-xl border border-gray-800 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-gray-900 text-gray-500 text-xs uppercase tracking-wider">
                <th className="text-left px-4 py-3">Market</th>
                <th className="text-left px-4 py-3">Outcome</th>
                <th className="text-right px-4 py-3">Shares</th>
                <th className="text-right px-4 py-3">Avg</th>
                <th className="text-right px-4 py-3">Current</th>
                <th className="text-right px-4 py-3">Value</th>
                <th className="text-right px-4 py-3">P&amp;L</th>
                <th className="text-right px-4 py-3">P&amp;L%</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800/50">
              {open.length === 0 ? (
                <tr><td colSpan={8} className="px-4 py-6 text-center text-gray-600 text-xs">No open paper positions</td></tr>
              ) : open.map((r, i) => (
                <tr key={i} className="bg-gray-950 hover:bg-gray-900/50 transition-colors">
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-2">
                      {r.icon && <img src={r.icon} alt="" className="w-5 h-5 rounded-full object-cover shrink-0" />}
                      {r.slug ? (
                        <a href={`https://polymarket.com/event/${r.slug}`} target="_blank" rel="noopener noreferrer"
                          className="text-gray-200 leading-tight line-clamp-1 hover:text-blue-400 hover:underline transition-colors"
                        >{r.market}</a>
                      ) : (
                        <span className="text-gray-200 leading-tight line-clamp-1">{r.market}</span>
                      )}
                    </div>
                  </td>
                  <td className="px-4 py-3 text-blue-300">{r.side}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-300">{r.shares.toFixed(1)}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-400">{fmtCents(r.entry)}</td>
                  <td className={`px-4 py-3 text-right tabular-nums font-medium ${r.currentPrice !== undefined ? 'text-gray-300' : 'text-gray-600'}`}>
                    {r.currentPrice !== undefined ? fmtCents(r.currentPrice) : '—'}
                  </td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-200">
                    {r.currentPrice !== undefined ? fmt$abs(r.shares * r.currentPrice) : '—'}
                  </td>
                  <td className={`px-4 py-3 text-right tabular-nums font-medium ${r.pnl === null ? 'text-gray-600' : r.pnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    <span className="whitespace-nowrap">{r.pnl === null ? '—' : fmt$(r.pnl)}</span>
                  </td>
                  <td className={`px-4 py-3 text-right tabular-nums ${r.pnlPct === null ? 'text-gray-600' : r.pnlPct >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    {r.pnlPct === null ? '—' : fmtPct(r.pnlPct)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      {/* Running P&L */}
      <section className="animate-fade-in">
        <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider mb-3">
          Running P&L by Trade
        </h2>
        <div className="rounded-xl border border-gray-800 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-gray-900 text-gray-500 text-xs uppercase tracking-wider">
                <th className="text-left px-4 py-3 w-10">#</th>
                <th className="text-left px-4 py-3 max-w-[11rem]">Market</th>
                <th className="text-left px-4 py-3">Outcome</th>
                <th className="text-right px-4 py-3">Shares</th>
                <th className="text-right px-4 py-3">Entry</th>
                <th className="text-right px-4 py-3">Cost</th>
                <th className="text-right px-4 py-3 w-32">P&amp;L</th>
                <th className="text-right px-4 py-3">Cumulative</th>
                <th className="text-center px-4 py-3">Status</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800/50">
              {runningRows.length === 0 ? (
                <tr><td colSpan={9} className="px-4 py-6 text-center text-gray-600 text-xs">No resolved paper trades yet</td></tr>
              ) : runningRows.map(row => (
                <tr key={row.index} className="bg-gray-950 hover:bg-gray-900/50 transition-colors">
                  <td className="px-4 py-3 text-gray-600 tabular-nums">{row.index}</td>
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-2">
                      {row.icon && <img src={row.icon} alt="" className="w-5 h-5 rounded-full object-cover shrink-0" />}
                      {row.slug ? (
                        <a href={`https://polymarket.com/event/${row.slug}`} target="_blank" rel="noopener noreferrer"
                          className="text-gray-200 line-clamp-1 hover:text-blue-400 hover:underline transition-colors"
                        >{row.title}</a>
                      ) : (
                        <span className="text-gray-200 line-clamp-1">{row.title}</span>
                      )}
                    </div>
                  </td>
                  <td className="px-4 py-3 text-blue-300 text-xs">{row.outcome}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-300">{row.size.toFixed(1)}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-400">{fmtCents(row.price)}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-400">{fmt$abs(row.cost)}</td>
                  <td className={`px-4 py-3 text-right tabular-nums font-medium ${row.pnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    <span className="whitespace-nowrap">{fmt$(row.pnl)}</span>
                  </td>
                  <td className={`px-4 py-3 text-right tabular-nums font-semibold ${row.cumulative >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    {fmt$(row.cumulative)}
                  </td>
                  <td className="px-4 py-3 text-center">
                    <StatusBadge status={row.status} />
                  </td>
                </tr>
              ))}
            </tbody>
            {runningRows.length > 0 && (
              <tfoot>
                <tr className="bg-gray-900 border-t border-gray-700">
                  <td colSpan={6} className="px-4 py-3 text-xs text-gray-500 uppercase tracking-wider">Totals</td>
                  <td className={`px-4 py-3 text-right tabular-nums font-bold ${cumulative >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    {fmt$(cumulative)}
                  </td>
                  <td className={`px-4 py-3 text-right tabular-nums font-bold ${cumulative >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    {fmt$(cumulative)}
                  </td>
                  <td />
                </tr>
              </tfoot>
            )}
          </table>
        </div>
      </section>
    </>
  );
}

function DashboardView({ positions, tradeRows, stats }: { positions: Position[]; tradeRows: TradeRow[]; stats: SessionStats }) {
  // tradeRows are already sorted oldest → newest, so count the streak from the end.
  const winStreak = currentWinStreak(tradeRows.map(r => r.status));
  return (
    <>
      {/* KPI Row 1 */}
      <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3 animate-fade-in">
        <KpiCard label="Total Trades"     value={String(stats.totalTrades)} />
        <KpiCard label="Win Rate"         value={fmtPct(stats.winRate)}        color="green" />
        <KpiCard label="Portfolio Value"  value={fmt$abs(stats.portfolioValue)} sub="open positions" />
        <KpiCard
          label="Total P&L"
          value={fmt$(stats.totalPnl)}
          sub={`${fmt$(stats.totalRealizedPnl, 2)} realized`}
          color={stats.totalPnl >= 0 ? 'green' : 'red'}
        />
        <KpiCard label="Largest Win"  value={fmt$(stats.largestWin)}  color="green" />
        <KpiCard label="Largest Loss" value={stats.largestLoss < 0 ? fmt$(stats.largestLoss) : '—'} color={stats.largestLoss < 0 ? 'red' : 'default'} />
      </div>

      {/* KPI Row 2 */}
      <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3 animate-fade-in">
        <KpiCard label="Wins"           value={String(stats.wins)}    color="green" />
        <KpiCard label="Losses"         value={String(stats.losses)}  color={stats.losses > 0 ? 'red' : 'default'} />
        <KpiCard label="Win Streak"     value={String(winStreak)} sub="current" color={winStreak > 0 ? 'green' : 'default'} />
        <KpiCard label="Total Fees"     value={fmt$abs(stats.totalFees)} sub={`${fmt$abs(stats.avgFeePerTrade)} avg/trade`} />
        <KpiCard
          label="Net P&L (after fees)"
          value={fmt$(stats.netPnl)}
          color={stats.netPnl >= 0 ? 'green' : 'red'}
        />
        <KpiCard label="Avg Return/Trade" value={fmt$(stats.avgReturn)} color={stats.avgReturn >= 0 ? 'green' : 'red'} />
      </div>

      {/* Active Positions */}
      {(() => {
        // Split positions into three buckets:
        //   open     — genuinely in-play (0.001 < curPrice < 0.998)
        //   resolved — settled as WIN (curPrice ≥ 0.998)
        //   dead     — token went to zero, awaiting official settlement
        const open     = positions.filter(p => p.curPrice > 0.001 && p.curPrice < 0.998);
        const resolved = positions.filter(p => p.curPrice >= 0.998);
        const dead     = positions.filter(p => p.curPrice <= 0.001);
        const visible  = [...resolved, ...open]; // wins first, then in-play

        const renderRow = (p: Position, i: number) => {
          const isResolved = p.curPrice >= 0.998;
          return (
            <tr key={i} className="bg-gray-950 hover:bg-gray-900/50 transition-colors">
              <td className="px-4 py-3">
                <div className="flex items-center gap-2">
                  {p.icon && (
                    <img src={p.icon} alt="" className="w-5 h-5 rounded-full object-cover shrink-0" />
                  )}
                  {p.slug ? (
                    <a
                      href={`https://polymarket.com/event/${p.slug}`}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-gray-200 leading-tight line-clamp-1 hover:text-blue-400 hover:underline transition-colors"
                    >{p.title}</a>
                  ) : (
                    <span className="text-gray-200 leading-tight line-clamp-1">{p.title}</span>
                  )}
                  {isResolved && (
                    <span className="text-xs text-emerald-400 bg-emerald-500/10 border border-emerald-500/20 px-1.5 py-0.5 rounded-full shrink-0">
                      Resolved ✓
                    </span>
                  )}
                </div>
              </td>
              <td className="px-4 py-3 text-blue-300">{p.outcome}</td>
              <td className="px-4 py-3 text-right tabular-nums text-gray-300">{p.size.toFixed(1)}</td>
              <td className="px-4 py-3 text-right tabular-nums text-gray-400">{fmtCents(p.avgPrice)}</td>
              <td className={`px-4 py-3 text-right tabular-nums font-medium ${isResolved ? 'text-emerald-400' : 'text-gray-300'}`}>
                {fmtCents(p.curPrice)}
              </td>
              <td className="px-4 py-3 text-right tabular-nums text-gray-200">{fmt$abs(p.currentValue)}</td>
              <td className={`px-4 py-3 text-right tabular-nums font-medium ${p.cashPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                {fmt$(p.cashPnl)}
              </td>
              <td className={`px-4 py-3 text-right tabular-nums ${p.percentPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                {p.percentPnl >= 0 ? '+' : ''}{p.percentPnl.toFixed(1)}%
              </td>
            </tr>
          );
        };

        return (
          <section className="animate-fade-in">
            <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider mb-3">
              Active Positions ({visible.length})
              {dead.length > 0 && (
                <span className="ml-2 text-xs text-red-500/70 normal-case tracking-normal font-normal">
                  +{dead.length} pending settlement at 0¢
                </span>
              )}
            </h2>
            <div className="rounded-xl border border-gray-800 overflow-hidden">
              <table className="w-full text-sm">
                <thead>
                  <tr className="bg-gray-900 text-gray-500 text-xs uppercase tracking-wider">
                    <th className="text-left px-4 py-3">Market</th>
                    <th className="text-left px-4 py-3">Outcome</th>
                    <th className="text-right px-4 py-3">Shares</th>
                    <th className="text-right px-4 py-3">Avg</th>
                    <th className="text-right px-4 py-3">Current</th>
                    <th className="text-right px-4 py-3">Value</th>
                    <th className="text-right px-4 py-3">P&L</th>
                    <th className="text-right px-4 py-3">P&L%</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-gray-800/50">
                  {visible.length > 0
                    ? visible.map(renderRow)
                    : <tr><td colSpan={8} className="px-4 py-6 text-center text-gray-600 text-xs">No open positions</td></tr>
                  }
                </tbody>
              </table>
            </div>
          </section>
        );
      })()}


      {/* Running P&L */}
      <section className="animate-fade-in">
        <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider mb-3">
          Running P&L by Trade <span className="text-gray-600 normal-case">(last 100 trades)</span>
        </h2>
        <div className="rounded-xl border border-gray-800 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-gray-900 text-gray-500 text-xs uppercase tracking-wider">
                <th className="text-left px-4 py-3 w-10">#</th>
                <th className="text-left px-4 py-3 max-w-[11rem]">Market</th>
                <th className="text-left px-4 py-3">Outcome</th>
                <th className="text-right px-4 py-3">Shares</th>
                <th className="text-right px-4 py-3">Entry</th>
                <th className="text-right px-4 py-3">Cost</th>
                <th className="text-right px-4 py-3 w-32">P&L</th>
                <th className="text-right px-4 py-3">Cumulative</th>
                <th className="text-center px-4 py-3">Status</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800/50">
              {/* Last 100 trades, newest first; row.index keeps the chronological number. */}
              {tradeRows.slice(-100).reverse().map((row) => (
                <tr key={row.index} className="bg-gray-950 hover:bg-gray-900/50 transition-colors">
                  <td className="px-4 py-3 text-gray-600 tabular-nums">{row.index}</td>
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-2">
                      {row.icon && (
                        <img src={row.icon} alt="" className="w-5 h-5 rounded-full object-cover shrink-0" />
                      )}
                      {row.slug ? (
                        <a
                          href={`https://polymarket.com/event/${row.slug}`}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="text-gray-200 line-clamp-1 hover:text-blue-400 hover:underline transition-colors"
                        >{row.title}</a>
                      ) : (
                        <span className="text-gray-200 line-clamp-1">{row.title}</span>
                      )}
                    </div>
                  </td>
                  <td className="px-4 py-3 text-blue-300 text-xs">{row.outcome}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-300">{row.size.toFixed(1)}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-400">{fmtCents(row.price)}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-400">{fmt$abs(row.cost)}</td>
                  <td className={`px-4 py-3 text-right tabular-nums font-medium ${row.pnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    <span className="whitespace-nowrap">{fmt$(row.pnl)}</span>
                  </td>
                  <td className={`px-4 py-3 text-right tabular-nums font-semibold ${row.cumulative >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    {fmt$(row.cumulative)}
                  </td>
                  <td className="px-4 py-3 text-center">
                    <StatusBadge status={row.status} />
                  </td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr className="bg-gray-900 border-t border-gray-700">
                <td colSpan={6} className="px-4 py-3 text-xs text-gray-500 uppercase tracking-wider">Totals</td>
                <td className={`px-4 py-3 text-right tabular-nums font-bold ${(tradeRows.at(-1)?.cumulative ?? 0) >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                  {fmt$(tradeRows.at(-1)?.cumulative ?? 0)}
                </td>
                <td className={`px-4 py-3 text-right tabular-nums font-bold ${(tradeRows.at(-1)?.cumulative ?? 0) >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                  {fmt$(tradeRows.at(-1)?.cumulative ?? 0)}
                </td>
                <td />
              </tr>
            </tfoot>
          </table>
        </div>
      </section>
    </>
  );
}

export default function App() {
  const [positions, setPositions] = useState<Position[]>([]);
  const [stats, setStats] = useState<SessionStats | null>(null);
  const [tradeRows, setTradeRows] = useState<TradeRow[]>([]);
  const [tradeLog, setTradeLog] = useState<TradeLogRow[]>([]);
  const [paperTrades, setPaperTrades] = useState<TradeLogRow[]>([]);
  const [view, setView] = useState<'dashboard' | 'paper' | 'tradelog' | 'kelly'>('dashboard');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const load = useCallback(async () => {
    try {
      setError(null);
      const [pos, act, rawPaper] = await Promise.all([fetchPositions(), fetchActivity(), fetchPaperTrades()]);
      const rows = buildTradeRows(pos, act);
      const rawStats = computeStats(pos, act);
      const buildPnl = rows.at(-1)?.cumulative ?? 0;
      setPositions(pos);
      setStats({ ...rawStats, totalPnl: buildPnl, netPnl: buildPnl - rawStats.totalFees });
      setTradeRows(rows);
      const realLog = buildTradeLogRows(pos, act);
      const merged = [...realLog, ...rawPaper]
        // True chronological order by actual order time; date as tiebreaker.
        .sort((a, b) => (a.ts - b.ts) || a.date.localeCompare(b.date))
        .map((row, i) => ({ ...row, num: i + 1 }));
      setTradeLog(merged);
      // Fetch live prices for open paper positions and merge in
      const priceMap = await fetchLivePrices(rawPaper);
      const paperWithPrices = rawPaper.map(t => {
        if (t.outcome !== 'NA' || !t.conditionId) return t;
        const currentPrice = priceMap[`${t.conditionId}:${t.side}`];
        if (currentPrice === undefined) return t;
        const currentValue = t.shares * currentPrice;
        const pnl         = currentValue - t.size;
        const pnlPct      = t.size > 0 ? pnl / t.size : null;
        return { ...t, currentPrice, pnl, pnlPct };
      });
      setPaperTrades(paperWithPrices);
      setLastUpdated(new Date());
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load data');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { load(); }, [load]);
  useEffect(() => {
    intervalRef.current = setInterval(load, REFRESH_INTERVAL);
    return () => { if (intervalRef.current) clearInterval(intervalRef.current); };
  }, [load]);

  const shortWallet = `${PROXY_WALLET.slice(0, 6)}…${PROXY_WALLET.slice(-4)}`;

  return (
    <div className="min-h-screen bg-gray-950 text-gray-100">
      <header className="border-b border-gray-800 py-5">
        <div className="max-w-7xl mx-auto px-4 sm:px-6 flex items-center justify-between">
          <div>
            <h1 className="text-xl font-bold">
              <span className="bg-gradient-to-r from-blue-400 to-emerald-400 bg-clip-text text-transparent">
                Trading Dashboard
              </span>
            </h1>
            <p className="text-sm text-gray-500 mt-1">
              w0kki1 · <span className="font-mono text-xs">{shortWallet}</span>
            </p>
          </div>
          <div className="flex items-center gap-3">
            <button
              onClick={() => setView(v => v === 'paper' ? 'dashboard' : 'paper')}
              className={`text-xs font-medium px-3 py-1.5 rounded-lg border transition-colors ${
                view === 'paper'
                  ? 'bg-cyan-600 border-cyan-500 text-white'
                  : 'bg-gray-800/50 border-gray-700 text-gray-300 hover:bg-gray-700 hover:text-white'
              }`}
            >
              Paper Trades
            </button>
            <button
              onClick={() => setView(v => v === 'tradelog' ? 'dashboard' : 'tradelog')}
              className={`text-xs font-medium px-3 py-1.5 rounded-lg border transition-colors ${
                view === 'tradelog'
                  ? 'bg-blue-600 border-blue-500 text-white'
                  : 'bg-gray-800/50 border-gray-700 text-gray-300 hover:bg-gray-700 hover:text-white'
              }`}
            >
              Trade Log
            </button>
            <button
              onClick={() => setView(v => v === 'kelly' ? 'dashboard' : 'kelly')}
              className={`text-xs font-medium px-3 py-1.5 rounded-lg border transition-colors ${
                view === 'kelly'
                  ? 'bg-blue-600 border-blue-500 text-white'
                  : 'bg-gray-800/50 border-gray-700 text-gray-300 hover:bg-gray-700 hover:text-white'
              }`}
            >
              KCC
            </button>
            {lastUpdated && (
              <span
                key={lastUpdated.getTime()}
                className="text-xs text-gray-500 animate-flash-update px-3 py-1 rounded-full bg-gray-800/50 border border-gray-800"
              >
                Updated {lastUpdated.toLocaleTimeString()}
              </span>
            )}
            <button
              onClick={load}
              className="text-xs font-medium px-3 py-1.5 rounded-lg border bg-gray-800/50 border-gray-700 text-gray-300 hover:bg-gray-700 hover:text-white transition-colors"
            >
              ↻ Refresh
            </button>
            <span className="text-xs text-gray-600 px-3 py-1 rounded-full bg-gray-800/50 border border-gray-800">
              ↻ 1h
            </span>
          </div>
        </div>
      </header>

      <main className="max-w-7xl mx-auto px-4 sm:px-6 py-6 space-y-6">
        {loading ? (
          <div className="text-center py-24 text-gray-500">
            <div className="text-4xl mb-4 animate-spin-3d inline-block">◎</div>
            <p className="text-sm">Loading your trades…</p>
          </div>
        ) : error ? (
          <div className="text-center py-24">
            <div className="text-red-400 font-medium mb-2">Failed to load data</div>
            <p className="text-sm text-gray-500 mb-4">{error}</p>
            <button onClick={load} className="px-5 py-2 bg-blue-600 text-white text-sm rounded-lg hover:bg-blue-500">
              Retry
            </button>
          </div>
        ) : stats ? (
          view === 'paper'
            ? <PaperDashboardView paperTrades={paperTrades} />
            : view === 'tradelog'
            ? <TradeLogView tradeLog={tradeLog} />
            : view === 'kelly'
            ? <KellyView stats={stats} tradeLog={tradeLog} />
            : <DashboardView positions={positions} tradeRows={tradeRows} stats={stats} />
        ) : null}
      </main>

      <footer className="border-t border-gray-800 px-4 py-4 mt-8">
        <p className="text-center text-xs text-gray-700">
          Live data from Polymarket · Not financial advice · For educational use
        </p>
      </footer>
    </div>
  );
}
