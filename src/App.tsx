import { useState, useEffect, useCallback, useRef } from 'react';
import type { Position, Activity, SessionStats, TradeRow } from './types';
import { fetchPositions, fetchActivity, computeStats, buildTradeRows, PROXY_WALLET } from './lib/polymarket';

const REFRESH_INTERVAL = 30_000;

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

export default function App() {
  const [positions, setPositions] = useState<Position[]>([]);
  const [activity, setActivity] = useState<Activity[]>([]);
  const [stats, setStats] = useState<SessionStats | null>(null);
  const [tradeRows, setTradeRows] = useState<TradeRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const load = useCallback(async () => {
    try {
      setError(null);
      const [pos, act] = await Promise.all([fetchPositions(), fetchActivity()]);
      setPositions(pos);
      setActivity(act);
      setStats(computeStats(pos, act));
      setTradeRows(buildTradeRows(pos, act));
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
      {/* Header */}
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
            {lastUpdated && (
              <span
                key={lastUpdated.getTime()}
                className="text-xs text-gray-500 animate-flash-update px-3 py-1 rounded-full bg-gray-800/50 border border-gray-800"
              >
                Updated {lastUpdated.toLocaleTimeString()}
              </span>
            )}
            <span className="text-xs text-gray-600 px-3 py-1 rounded-full bg-gray-800/50 border border-gray-800">
              ↻ 30s
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
