import type {
  Account,
  Activity,
  Asset,
  Candle,
  ChartRange,
  DecepticonWatchlist,
  IntervalRank,
  KeyCheck,
  MarketMovers,
  MoversNews,
  NewsItem,
  Order,
  OrderRequest,
  Position,
  Pressure,
  PublicConfig,
  QuantResponse,
  Readiness,
  ScanBar,
  ScanState,
  StockNews,
  SymbolMeta,
  VwapPoint,
} from "../types";

// All requests go through the Vite dev proxy (same origin), so no base URL needed.
async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    const msg = (body as { error?: string }).error || res.statusText;
    throw new Error(msg);
  }
  return body as T;
}

export const api = {
  config: () => req<PublicConfig>("/api/config"),
  keycheck: () => req<KeyCheck>("/api/keycheck"),
  account: () => req<Account>("/api/account"),
  positions: () => req<Position[]>("/api/positions"),
  orders: () => req<Order[]>("/api/orders"),
  assets: () => req<Asset[]>("/api/assets"),
  placeOrder: (o: OrderRequest) =>
    req<Order>("/api/orders", { method: "POST", body: JSON.stringify(o) }),

  // Static historical bars for the 1W/1M/6M/1Y chart ranges (works for any symbol).
  history: (symbol: string, range: ChartRange) =>
    req<{ symbol: string; range: ChartRange; bars: Candle[] }>(
      `/api/history?symbol=${encodeURIComponent(symbol)}&range=${range}`
    ),
  cancelOrder: (id: string) => req<{ status: string }>(`/api/orders/${id}`, { method: "DELETE" }),
  cancelAll: () => req<{ status: string }>("/api/orders", { method: "DELETE" }),

  // Execution symbol management
  addExecSymbol: (symbol: string) =>
    req<{ symbol: string; added: boolean; all: string[] }>("/api/execution/symbols", {
      method: "POST",
      body: JSON.stringify({ symbol }),
    }),
  removeExecSymbol: (symbol: string, both = false) =>
    req<{ symbol: string; removed: boolean; all: string[] }>(
      `/api/execution/symbols/${encodeURIComponent(symbol)}${both ? "?both=1" : ""}`,
      { method: "DELETE" }
    ),

  // Trade history (fills from Alpaca)
  activities: (days = 30, limit = 200) =>
    req<Activity[]>(`/api/activities?days=${days}&limit=${limit}`),

  // All fills in a window (paginated past the 100 cap) — for the Metrics page.
  fills: (days = 120) => req<Activity[]>(`/api/fills?days=${days}`),

  // News headlines (with sentiment) and order-flow (buy/sell pressure).
  news: (symbols: string[], limit = 10) =>
    req<NewsItem[]>(`/api/news?symbols=${encodeURIComponent(symbols.join(","))}&limit=${limit}`),
  pressure: (symbol: string) => req<Pressure>(`/api/pressure?symbol=${encodeURIComponent(symbol)}`),
  rvol: (symbol: string) =>
    req<{ symbol: string; rvol: number; available: boolean }>(`/api/rvol?symbol=${encodeURIComponent(symbol)}`),

  // Trading readiness
  readiness: () => req<Readiness>("/api/readiness"),

  // Watchlist page
  watchlistSymbols: () => req<{ symbols: string[] }>("/api/watchlist/symbols"),
  addWatchSymbol: (symbol: string) =>
    req<{ symbol: string; added: boolean; symbols: string[] }>("/api/watchlist/symbols", {
      method: "POST",
      body: JSON.stringify({ symbol }),
    }),
  removeWatchSymbol: (symbol: string) =>
    req<{ symbol: string; removed: boolean; symbols: string[] }>(
      `/api/watchlist/symbols/${encodeURIComponent(symbol)}`,
      { method: "DELETE" }
    ),
  openingAnalysis: (top = 15, scope?: "execution") =>
    req<IntervalRank[]>(`/api/opening-analysis?top=${top}${scope ? `&scope=${scope}` : ""}`),
  assetNames: (symbols: string[]) =>
    req<Record<string, string>>(`/api/asset-names?symbols=${encodeURIComponent(symbols.join(","))}`),
  symbolMeta: (symbols: string[]) =>
    req<Record<string, SymbolMeta>>(`/api/symbol-meta?symbols=${encodeURIComponent(symbols.join(","))}`),
  searchAssets: (q: string, limit = 20) =>
    req<Asset[]>(`/api/assets/search?q=${encodeURIComponent(q)}&limit=${limit}`),
  movers: (top = 50) => req<MarketMovers>(`/api/movers?top=${top}`),
  quotesSnapshot: () => req<Record<string, { price: number; ref: number }>>(`/api/quotes`),
  moversNews: (top = 12) => req<MoversNews>(`/api/movers-news?top=${top}`),
  stockNews: (symbol: string) =>
    req<StockNews>(`/api/stock-news?symbol=${encodeURIComponent(symbol)}`),

  // Quant pipeline (dip-driven AI team) on the Claude paper account.
  quant: () => req<QuantResponse>("/api/quant"),

  // DECEPTICON
  decepticonWatchlist: () => req<DecepticonWatchlist>("/api/decepticon/watchlist"),
  decepticonScan: () => req<ScanState[]>("/api/decepticon/scan"),
  decepticonBars: (symbol: string) =>
    req<{ symbol: string; bars: ScanBar[]; vwap: VwapPoint[] }>(
      `/api/decepticon/bars?symbol=${encodeURIComponent(symbol)}`
    ),
};
