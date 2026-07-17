export interface Candle {
  time: number; // unix seconds
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
}

export interface CandleUpdate {
  symbol: string;
  timeframe: number;
  candle: Candle;
}

// Historical lookback ranges (static REST bars) vs. live intraday timeframes. A chart's
// "view" is one of the intraday strings or one of these ranges.
export type ChartRange = "1W" | "1M" | "6M" | "1Y";
export type ChartView = "1m" | "5m" | "10m" | ChartRange;

export const CHART_RANGES: ChartRange[] = ["1W", "1M", "6M", "1Y"];

// isRange narrows a view to a historical range (vs. a live intraday timeframe).
export function isRange(v: ChartView): v is ChartRange {
  return v === "1W" || v === "1M" || v === "6M" || v === "1Y";
}

// viewToTimeframe maps an intraday view ("1m"/"5m"/"10m") to its minute count. Ranges
// return 0 (they don't use the live timeframe stream).
export function viewToTimeframe(v: ChartView): number {
  return v === "1m" ? 1 : v === "5m" ? 5 : v === "10m" ? 10 : 0;
}

export interface Quote {
  symbol: string;
  price: number;
  time: number;
}

export interface Account {
  account_number: string;
  status: string;
  currency: string;
  equity: number;
  last_equity: number;
  buying_power: number;
  cash: number;
  portfolio_value: number;
  pattern_day_trader: boolean;
  trading_blocked: boolean;
}

export interface Position {
  symbol: string;
  qty: number;
  side: string;
  avg_entry_price: number;
  current_price: number;
  market_value: number;
  cost_basis: number;
  unrealized_pl: number;
  unrealized_plpc: number;
}

export interface Order {
  id: string;
  symbol: string;
  side: string;
  type: string;
  qty: string;
  notional: string;
  filled_qty: string;
  filled_avg_price: string;
  limit_price: string;
  stop_price: string;
  order_class: string;
  time_in_force: string;
  status: string;
  submitted_at: string;
}

export interface Asset {
  symbol: string;
  name: string;
  class: string;
  exchange: string;
  tradable: boolean;
  fractionable: boolean;
  shortable: boolean;
}

export interface PublicConfig {
  symbols: string[];
  base_symbols: string[];
  added_symbols: string[];
  mode: "LIVE" | "PAPER";
  feed: string;
  timeframes: number[];
  max_order_notional: number;
  fractionable: Record<string, boolean>;
  decepticon_enabled: boolean;
  sip_degraded: boolean;
}

export interface Activity {
  id: string;
  time: string;
  symbol: string;
  side: string;
  qty: number;
  price: number;
  value: number;
  type: string;
  order_id: string;
}

export interface NewsItem {
  id: number;
  headline: string;
  summary: string;
  author: string;
  url: string;
  symbols: string[];
  created_at: string;
  sentiment: "positive" | "negative" | "neutral";
}

export interface SymbolMeta {
  name: string;
  sector: string;
}

export interface MarketMover {
  symbol: string;
  price: number;
  change: number;
  percent_change: number;
}
export interface MarketMovers {
  gainers: MarketMover[];
  losers: MarketMover[];
}

// Each mover tagged with a cheap Alpaca-only news read (drives the DIP?/KNIFE badges).
export interface MoverNews {
  symbol: string;
  price: number;
  change: number;
  percent_change: number;
  direction: string; // "gainer" | "loser"
  has_catalyst: boolean;
  sentiment: string; // positive | negative | neutral | none
  why: string;
}
export interface MoversNews {
  gainers: MoverNews[];
  losers: MoverNews[];
  as_of: string;
}

// One stock's news + the on-click Gemini "why is it moving" summary (the dropdown).
export interface NewsHeadline {
  source: string; // "Benzinga"
  headline: string;
  url: string;
  created_at: string;
  sentiment: string; // positive | negative | neutral | ""
}
export interface StockNews {
  symbol: string;
  summary: string;
  summary_status: string; // ok | no_news | disabled | budget | busy | error
  sentiment: string;
  has_catalyst: boolean;
  headlines: NewsHeadline[];
  as_of: string;
}

export interface Pressure {
  symbol: string;
  buy_vol: number;
  sell_vol: number;
  buy_pct: number;
  roll_buy_vol: number;
  roll_sell_vol: number;
  roll_buy_pct: number;
  window_min: number;
}

export type OrderType = "market" | "limit" | "stop" | "stop_limit" | "trailing_stop";
export type OrderClass = "simple" | "bracket" | "oco" | "oto";

export interface OrderRequest {
  symbol: string;
  side: "buy" | "sell";
  type: OrderType;
  qty?: number;
  notional?: number;
  limit_price?: number;
  stop_price?: number;
  trail_price?: number;
  trail_percent?: number;
  time_in_force?: string;
  extended_hours?: boolean;
  order_class?: OrderClass;
  take_profit_limit?: number;
  stop_loss_stop?: number;
  stop_loss_limit?: number;
}

export interface Readiness {
  can_trade: boolean;
  account_status: string;
  mode: string;
  trading_blocked: boolean;
  account_blocked: boolean;
  trade_suspended_by_user: boolean;
  shorting_enabled: boolean;
  pattern_day_trader: boolean;
  daytrade_count: number;
  buying_power: number;
  cash: number;
  equity: number;
  market_open: boolean;
  next_open: string;
  next_close: string;
  issues: string[];
}

export interface KeyCheck {
  keys_valid: boolean;
  mode: string;
  account_number: string;
  account_status: string;
  trading_blocked: boolean;
  sip_entitled: boolean;
  configured_feed: string;
  detail: string;
}

export interface TradeUpdate {
  event: string; // new | fill | partial_fill | canceled | rejected | ...
  symbol: string;
  side: string;
  qty: string;
  price: number;
  status: string;
  at: string;
}

// ---- DECEPTICON scanner ----
export interface WatchTicker {
  symbol: string;
  company: string;
  type: string;
  catalyst: string;
}
export interface Department {
  name: string;
  slug: string;
  icon: string;
  tickers: WatchTicker[];
}
export interface DecepticonWatchlist {
  departments: Department[];
  symbol_count: number;
  feed: string;
  sip_degraded: boolean;
}
export interface ScanState {
  symbol: string;
  price: number;
  prev_close: number;
  open: number;
  chg_close_pct: number;
  chg_open_pct: number;
  or5_pct: number;
  or15_pct: number;
  or20_pct: number;
  volume: number;
  avg_volume: number;
  rvol: number;
  vwap: number;
  day_high: number;
  day_low: number;
  spread: number;
  catalyst: string;
  has_bars: boolean;
  updated: number;
}
export interface ScanBar {
  time: number;
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
  vwap: number;
}
export interface VwapPoint {
  time: number;
  value: number;
}

export interface Mover {
  symbol: string;
  open: number;
  price: number;
  pct: number;
}
export interface IntervalRank {
  minutes: number;
  elapsed: boolean;
  rising: Mover[];
  falling: Mover[];
}

// --- Quant pipeline (Paper · Claude page) ---
export interface QuantAllocSnapshot {
  budget: number; // effective (capped at real account equity)
  configured_max: number; // target budget before the equity cap
  account_equity: number; // real paper-account equity (0 = unknown)
  free: number;
  deployed: number;
  open_count: number;
  max_concurrent: number;
  per_position: number;
  positions: Record<string, number>;
}
export interface QuantPosition {
  symbol: string;
  qty: number;
  entry_price: number;
  entry_time: string;
  mark_price: number;
  unrealized_pnl: number;
}
export interface QuantTrade {
  symbol: string;
  entry_time: string;
  exit_time: string;
  entry_price: number;
  exit_price: number;
  qty: number;
  pnl: number;
  exit_reason: string;
}
export interface QuantStateT {
  realized_pnl: number;
  unrealized_pnl: number;
  win_rate: number;
  total_trades: number;
  positions: QuantPosition[];
  trades: QuantTrade[];
}
export interface ReasonStat {
  count: number;
  total_pnl: number;
  avg_pnl: number;
}
export interface ExitAttribution {
  by_reason: Record<string, ReasonStat>;
  discretionary_avg_pnl: number;
  discretionary_count: number;
  stop_avg_pnl: number;
  stop_count: number;
  agent3_adds_value: boolean;
}
export interface QuantReview {
  date: string;
  generated_at: string;
  summary: string;
  realized_pnl: number;
  win_rate: number;
  total_trades: number;
  what_worked: string[];
  what_didnt: string[];
  suggested_changes: string[];
  consistency_score: number;
}
export interface SourceStat {
  trades: number;
  wins: number;
  win_rate: number;
  total_pnl: number;
  avg_pnl: number;
}
export interface DipScorecard {
  window_days: number;
  decisions: number;
  approved: number;
  rejected: number;
  avg_confidence: number;
  dip: SourceStat;
  signal: SourceStat;
  rise: SourceStat;
  rehydrated: SourceStat;
  knife_rate: number;
  verdict: string;
}
export interface AgentInfo {
  name: string;
  model: string;
  role: string;
  live: boolean;
}
// One desk (paper account) of the quant team: "signal" and "dip+rise" each run their
// own account, allocator, and daily loss cap.
export interface QuantDeskReport {
  name: string;
  live: boolean;
  alloc: QuantAllocSnapshot;
  account_day_pnl: number; // Alpaca: equity − prior close (broker truth)
  realized_pnl: number;
  unrealized_pnl: number;
  trades: number;
}
export interface QuantReport {
  live: boolean;
  universe_size: number;
  posture: string;
  alloc: QuantAllocSnapshot;
  state: QuantStateT; // whole team (both desk accounts merged)
  desks?: QuantDeskReport[];
  attribution: ExitAttribution;
  dip_score?: DipScorecard;
  agents?: AgentInfo[];
  review?: QuantReview;
}
export interface QuantResponse {
  enabled: boolean;
  report?: QuantReport;
}

// ---- Dip+Rise desk (Agent 2 dips + rise watcher, its own paper account) ----
export interface RiseArmView {
  symbol: string;
  armed_at: string;
  dip_price: number;
  dip_low: number;
  confirm_level: number;
  expires_in_sec: number;
  agent2_conf: number;
}
export interface DipRiseEvent {
  time: string;
  agent: string;
  event: string;
  symbol: string;
  note: string;
}
export interface DipRiseReport {
  enabled: boolean;
  live: boolean;
  rise_live: boolean;
  alloc: QuantAllocSnapshot;
  state: QuantStateT;
  dip_score?: DipScorecard;
  armed: RiseArmView[];
  events: DipRiseEvent[];
}

// Eval scoreboard (backend/internal/evals/evals.go) — rolling per-strategy counterfactual
// expectancy + CUSUM watchdog + LLM-judge calibration. Served at GET /api/evals; the
// handler returns {enabled:false} until the first computation completes.
export interface StrategyRow {
  strategy: string;
  signals: number;
  outcomes: number;
  mean_r: number;
  traded: number;
  cusum_alarm: boolean;
  demoted: boolean;
  reason?: string;
}
export interface JudgeCalib {
  decisions: number;
  approved: number;
  vetoed: number;
  joined: number;
  approved_mean_r: number;
  vetoed_mean_r: number;
  veto_value_r: number;
  brier: number;
}
export interface Scoreboard {
  enabled?: boolean;
  generated_at: string;
  window_days: number;
  strategies: StrategyRow[] | null;
  judge: JudgeCalib;
  demoted_set: string[] | null;
}

// ---- RIDP (Rider & Dipper two-strategy paper desk) ----
export interface RidpPosition {
  strategy: "rider" | "dipper";
  symbol: string;
  qty: number;
  entry: number;
  opened_at: string;
  peak: number;
  hard_stop: number;
  atr: number;
  tightened: boolean;
  sessions: number;
  last: number;
  unrealized: number;
  trail_level: number;
}
export interface RidpTrade {
  strategy: string;
  symbol: string;
  qty: number;
  entry: number;
  exit: number;
  pnl: number;
  reason: string;
  opened_at: string;
  closed_at: string;
}
export interface RidpStratStats {
  trades: number;
  wins: number;
  win_rate: number;
  realized_pnl: number;
  avg_pnl: number;
  today_pnl: number;
}
export interface RidpReport {
  enabled: boolean;
  live: boolean;
  account_equity: number;
  account_last_equity: number; // Alpaca: equity at prior close
  account_day_pnl: number; // Alpaca: equity − last_equity (broker truth)
  buying_power: number;
  deployed: number;
  open: RidpPosition[] | null;
  rider: RidpStratStats;
  dipper: RidpStratStats;
  dipper_setups: string[] | null;
  dipper_triggered: string[] | null;
  closed: RidpTrade[] | null;
  universe_size: number;
  reverter_open: RidpPosition[] | null;
  reverter: RidpStratStats;
}

export type WsMessage =
  | { type: "candle"; data: CandleUpdate }
  | { type: "quote"; data: Quote }
  | { type: "snapshot"; data: { symbol: string; timeframe: number; candles: Candle[] } }
  | { type: "account"; data: Account }
  | { type: "positions"; data: Position[] }
  | { type: "orders"; data: Order[] }
  | { type: "trade_update"; data: TradeUpdate }
  | { type: "scan"; data: ScanState[] }
  | { type: "exec_symbols"; data: string[] }
  | { type: "watch_symbols"; data: string[] }
  | { type: "error"; data: unknown };
