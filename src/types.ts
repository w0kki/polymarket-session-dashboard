export interface Position {
  conditionId: string;
  title: string;
  outcome: string;
  size: number;
  avgPrice: number;
  curPrice: number;
  initialValue: number;
  currentValue: number;
  cashPnl: number;
  percentPnl: number;
  icon: string;
  slug: string;
  endDate: string;
  redeemable: boolean;
}

export interface Activity {
  conditionId: string;
  timestamp: number;
  type: 'TRADE' | 'REDEEM';
  side?: 'BUY' | 'SELL';
  title: string;
  outcome: string;
  size: number;
  usdcSize: number;
  price: number;
  icon: string;
  slug: string;
}

export interface TradeRow {
  index: number;
  title: string;
  outcome: string;
  size: number;
  price: number;
  cost: number;
  pnl: number;
  cumulative: number;
  status: 'WIN' | 'LOSS' | 'ACTIVE' | 'SOLD';
  timestamp: number;
  icon: string;
}

export interface TradeLogRow {
  num: number;           // A
  date: string;          // B
  market: string;        // C
  sport: string;         // D
  type: string;          // E
  side: string;          // F
  entry: number;         // G
  shares: number;        // H
  size: number;          // I  = G × H
  exit: number | null;   // J
  outcome: string;       // K
  pnl: number | null;    // L  = (J - G) × H
  pnlPct: number | null; // M  = L / I
  notes: string;         // N
  feeCat: string;        // O
  buyFee: number;        // P
  sellFee: number;       // Q
  totalFees: number;     // R  = P + Q
  netPnl: number | null; // S  = L - R
  icon: string;
}

export interface SessionStats {
  totalTrades: number;
  portfolioValue: number;
  unrealizedPnl: number;
  totalRealizedPnl: number;
  totalPnl: number;
  winRate: number;
  wins: number;
  losses: number;
  activePositions: number;
  largestWin: number;
  largestLoss: number;
  avgReturn: number;
  totalFees: number;
  netPnl: number;
  avgFeePerTrade: number;
}
