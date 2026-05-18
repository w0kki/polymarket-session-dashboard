import { useState, useEffect, useCallback, useRef } from 'react';
import type { Position, SessionStats, TradeRow, TradeLogRow } from './types';
import { fetchPositions, fetchActivity, computeStats, buildTradeRows, buildTradeLogRows, PROXY_WALLET } from './lib/polymarket';

const REFRESH_INTERVAL = 60 * 60 * 1000;

const fmt$ = (n: number, decimals = 2) =>
  (n >= 0 ? '+' : '') + n.toLocaleString('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: decimals, maximumFractionDigits: decimals });
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
  const lossPnls = tradeLog.filter(r => r.outcome === 'LOSS' && r.pnl !== null).map(r => Math.abs(r.pnl!));
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

function TradeLogView({ tradeLog, stats }: { tradeLog: TradeLogRow[]; stats: SessionStats }) {
  return (
    <section className="animate-fade-in">
      <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider mb-3">
        Trade Log ({tradeLog.length} trades)
      </h2>
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
            {tradeLog.map((row) => (
              <tr key={row.num} className="bg-gray-950 hover:bg-gray-900/50 transition-colors">
                <td className="px-3 py-2.5 text-right text-gray-600 tabular-nums">{row.num}</td>
                <td className="px-3 py-2.5 text-gray-400 tabular-nums">{row.date}</td>
                <td className="px-3 py-2.5">
                  <div className="flex items-center gap-2">
                    {row.icon && (
                      <img src={row.icon} alt="" className="w-4 h-4 rounded-full object-cover shrink-0" />
                    )}
                    <span className="text-gray-200 line-clamp-1 max-w-48">{row.market}</span>
                  </div>
                </td>
                <td className="px-3 py-2.5 text-gray-400">{row.sport}</td>
                <td className="px-3 py-2.5">
                  <span className={`text-xs px-1.5 py-0.5 rounded ${row.type === 'Latency Arb' ? 'text-purple-400 bg-purple-500/10' : 'text-orange-400 bg-orange-500/10'}`}>
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
            <tr className="bg-gray-900 border-t border-gray-700">
              <td colSpan={10} className="px-3 py-3 text-xs text-gray-500 uppercase tracking-wider">Totals</td>
              <td className={`px-3 py-3 text-right tabular-nums font-bold ${stats.totalPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                {fmt$(stats.totalPnl)}
              </td>
              <td />
              <td />
              <td />
              <td className="px-3 py-3 text-right tabular-nums font-bold text-gray-400 text-xs">
                {fmt$abs(stats.totalFees)}
              </td>
              <td />
              <td className="px-3 py-3 text-right tabular-nums font-bold text-xs">
                {fmt$abs(stats.totalFees)}
              </td>
              <td className={`px-3 py-3 text-right tabular-nums font-bold text-xs ${stats.netPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                {fmt$(stats.netPnl)}
              </td>
            </tr>
          </tfoot>
        </table>
      </div>
    </section>
  );
}

function DashboardView({ positions, tradeRows, stats }: { positions: Position[]; tradeRows: TradeRow[]; stats: SessionStats }) {
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
      <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-3 animate-fade-in">
        <KpiCard label="Wins"           value={String(stats.wins)}    color="green" />
        <KpiCard label="Losses"         value={String(stats.losses)}  color={stats.losses > 0 ? 'red' : 'default'} />
        <KpiCard label="Total Fees"     value={fmt$abs(stats.totalFees)} sub={`${fmt$abs(stats.avgFeePerTrade)} avg/trade`} />
        <KpiCard
          label="Net P&L (after fees)"
          value={fmt$(stats.netPnl)}
          color={stats.netPnl >= 0 ? 'green' : 'red'}
        />
        <KpiCard label="Avg Return/Trade" value={fmt$(stats.avgReturn)} color={stats.avgReturn >= 0 ? 'green' : 'red'} />
      </div>

      {/* Active Positions */}
      <section className="animate-fade-in">
        <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider mb-3">
          Active Positions ({positions.length})
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
              {positions.map((p, i) => {
                const isResolved = p.curPrice >= 0.998;
                return (
                  <tr key={i} className="bg-gray-950 hover:bg-gray-900/50 transition-colors">
                    <td className="px-4 py-3">
                      <div className="flex items-center gap-2">
                        {p.icon && (
                          <img src={p.icon} alt="" className="w-5 h-5 rounded-full object-cover shrink-0" />
                        )}
                        <span className="text-gray-200 leading-tight line-clamp-1">{p.title}</span>
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
              })}
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
                <th className="text-left px-4 py-3">Market</th>
                <th className="text-left px-4 py-3">Outcome</th>
                <th className="text-right px-4 py-3">Shares</th>
                <th className="text-right px-4 py-3">Entry</th>
                <th className="text-right px-4 py-3">Cost</th>
                <th className="text-right px-4 py-3">P&L</th>
                <th className="text-right px-4 py-3">Cumulative</th>
                <th className="text-center px-4 py-3">Status</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-800/50">
              {tradeRows.map((row) => (
                <tr key={row.index} className="bg-gray-950 hover:bg-gray-900/50 transition-colors">
                  <td className="px-4 py-3 text-gray-600 tabular-nums">{row.index}</td>
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-2">
                      {row.icon && (
                        <img src={row.icon} alt="" className="w-5 h-5 rounded-full object-cover shrink-0" />
                      )}
                      <span className="text-gray-200 line-clamp-1">{row.title}</span>
                    </div>
                  </td>
                  <td className="px-4 py-3 text-blue-300 text-xs">{row.outcome}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-300">{row.size.toFixed(1)}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-400">{fmtCents(row.price)}</td>
                  <td className="px-4 py-3 text-right tabular-nums text-gray-400">{fmt$abs(row.cost)}</td>
                  <td className={`px-4 py-3 text-right tabular-nums font-medium ${row.pnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                    {fmt$(row.pnl)}
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
                <td className={`px-4 py-3 text-right tabular-nums font-bold ${stats.totalPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
                  {fmt$(stats.totalPnl)}
                </td>
                <td className={`px-4 py-3 text-right tabular-nums font-bold ${stats.totalPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
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
  const [view, setView] = useState<'dashboard' | 'tradelog' | 'kelly'>('dashboard');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const load = useCallback(async () => {
    try {
      setError(null);
      const [pos, act] = await Promise.all([fetchPositions(), fetchActivity()]);
      setPositions(pos);
      setStats(computeStats(pos, act));
      setTradeRows(buildTradeRows(pos, act));
      setTradeLog(buildTradeLogRows(pos, act));
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
          view === 'tradelog'
            ? <TradeLogView tradeLog={tradeLog} stats={stats} />
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
