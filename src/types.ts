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
