import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api } from "./api/client";
import { useWebSocket } from "./hooks/useWebSocket";
import { Chart } from "./components/Chart";
import { ChartOrderPopup, DRAW_MARKS, type DrawType } from "./components/ChartOrderPopup";
import { Watchlist } from "./components/Watchlist";
import { OrderPanel } from "./components/OrderPanel";
import { ConfirmModal } from "./components/ConfirmModal";
import { Positions } from "./components/Positions";
import { Header } from "./components/Header";
import { NewsPanel } from "./components/NewsPanel";
import { StrategyBadge } from "./components/StrategyBadge";
import { RangeToggle } from "./components/RangeToggle";
import { basisBySymbol } from "./costBasis";
import { bollinger, rsi, evaluate } from "./indicators";
import { useHistoryBars, mergeLastBar } from "./hooks/useHistoryBars";
import { isRange, viewToTimeframe } from "./types";
import type {
  Account,
  Activity,
  Candle,
  ChartView,
  Order,
  OrderRequest,
  Position,
  PublicConfig,
  Quote,
  WsMessage,
} from "./types";

// Short local timezone label (e.g. "GMT+1") shown next to the chart so it's explicit
// that timestamps are in the user's local time.
const TZ_LABEL =
  new Intl.DateTimeFormat([], { timeZoneName: "short" })
    .formatToParts(new Date())
    .find((p) => p.type === "timeZoneName")?.value ?? "local";

// Merge a streamed candle into a series array (replace-last / append / ignore-stale).
function upsert(arr: Candle[], c: Candle): Candle[] {
  if (arr.length === 0) return [c];
  const last = arr[arr.length - 1];
  if (c.time === last.time) {
    const copy = arr.slice();
    copy[copy.length - 1] = c;
    return copy;
  }
  if (c.time > last.time) return [...arr, c];
  return arr; // stale tick
}

export default function ExecutionEngine() {
  const [cfg, setCfg] = useState<PublicConfig | null>(null);
  const [sipEntitled, setSipEntitled] = useState<boolean | null>(null);
  const [symbol, setSymbol] = useState<string>("");
  const [view, setView] = useState<ChartView>("1m");
  // A historical range (1W/1M/6M/1Y) shows static REST bars; intraday views stream live.
  // While a range is shown, the live stream stays warm at 1m for instant switch-back.
  const range = isRange(view) ? view : null;
  const timeframe = range ? 1 : viewToTimeframe(view);
  const [showIndicators, setShowIndicators] = useState<boolean>(
    () => localStorage.getItem("lo.indicators") !== "off"
  );

  function toggleIndicators() {
    setShowIndicators((on) => {
      const next = !on;
      localStorage.setItem("lo.indicators", next ? "on" : "off");
      return next;
    });
  }

  const seriesRef = useRef<Map<string, Candle[]>>(new Map());
  const [activeCandles, setActiveCandles] = useState<Candle[]>([]);
  const [quotes, setQuotes] = useState<Record<string, Quote>>({});
  const refPrices = useRef<Record<string, number>>({});
  const [account, setAccount] = useState<Account | null>(null);
  const [positions, setPositions] = useState<Position[]>([]);
  const [orders, setOrders] = useState<Order[]>([]);
  const [fills, setFills] = useState<Activity[]>([]);

  const [rvol, setRvol] = useState<{ value: number; available: boolean } | null>(null);
  const [drawMode, setDrawMode] = useState(false);
  const [draftPrice, setDraftPrice] = useState<number | null>(null);
  const [draftType, setDraftType] = useState<DrawType | null>(null);
  const [pending, setPending] = useState<{ req: OrderRequest; est: number } | null>(null);
  const [placing, setPlacing] = useState(false);
  const [toast, setToast] = useState<string>("");
  const toastTimer = useRef<number>(0);
  const [resetNonce, setResetNonce] = useState(0);

  const activeKey = `${symbol}|${timeframe}`;
  const activeKeyRef = useRef(activeKey);
  activeKeyRef.current = activeKey;

  const handleMessage = useCallback((m: WsMessage) => {
    switch (m.type) {
      case "snapshot": {
        const key = `${m.data.symbol}|${m.data.timeframe}`;
        const candles = m.data.candles ?? [];
        seriesRef.current.set(key, candles);
        if (candles.length > 0) refPrices.current[m.data.symbol] = candles[0].close;
        if (key === activeKeyRef.current) setActiveCandles(candles);
        break;
      }
      case "candle": {
        const key = `${m.data.symbol}|${m.data.timeframe}`;
        const prev = seriesRef.current.get(key) ?? [];
        const next = upsert(prev, m.data.candle);
        seriesRef.current.set(key, next);
        if (key === activeKeyRef.current) setActiveCandles(next);
        break;
      }
      case "quote": {
        setQuotes((q) => ({ ...q, [m.data.symbol]: m.data }));
        break;
      }
      case "account":
        setAccount(m.data);
        break;
      case "positions":
        setPositions(m.data);
        break;
      case "orders":
        setOrders(m.data);
        break;
      case "exec_symbols":
        // A stock was added/removed (e.g. via the global search) — refresh config so
        // the sidebar list and fractionable flags update live.
        api.config().then(setCfg).catch(() => {});
        break;
      case "trade_update": {
        const u = m.data;
        const px = u.price > 0 ? ` @ $${u.price.toFixed(2)}` : "";
        setToast(`${u.event.replace("_", " ").toUpperCase()}: ${u.side?.toUpperCase()} ${u.qty} ${u.symbol}${px}`);
        window.clearTimeout(toastTimer.current);
        toastTimer.current = window.setTimeout(() => setToast(""), 6000);
        break;
      }
    }
  }, []);

  const { status, send } = useWebSocket(handleMessage);

  // Load public config + keycheck once.
  useEffect(() => {
    api.config().then((c) => {
      setCfg(c);
      if (c.symbols.length > 0) setSymbol(c.symbols[0]);
    });
    api.keycheck().then((k) => setSipEntitled(k.sip_entitled)).catch(() => setSipEntitled(null));
  }, []);

  // Subscribe whenever symbol/timeframe changes (and on (re)connect).
  useEffect(() => {
    if (!symbol) return;
    const cached = seriesRef.current.get(activeKey);
    setActiveCandles(cached ?? []);
    if (status === "open") send({ action: "subscribe", symbol, timeframe });
  }, [symbol, timeframe, status, activeKey, send]);

  // Clear any pending visual-order draft when the symbol changes (don't carry a level over).
  useEffect(() => {
    setDraftPrice(null);
    setDraftType(null);
  }, [symbol]);

  const lastPrice = quotes[symbol]?.price ?? activeCandles[activeCandles.length - 1]?.close ?? 0;
  const fractionable = cfg?.fractionable?.[symbol] ?? false;

  // Relative volume for the selected symbol (from the scanner's time-of-day-aware RVOL).
  // Refetched on symbol change and every 15s; "n/a" when the symbol isn't in the scanner.
  useEffect(() => {
    if (!symbol) return;
    let alive = true;
    const load = () =>
      api
        .rvol(symbol)
        .then((r) => alive && setRvol({ value: r.rvol, available: r.available }))
        .catch(() => alive && setRvol(null));
    setRvol(null);
    load();
    const id = window.setInterval(load, 15000);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, [symbol]);

  // Historical ranges load static REST bars; the latest bar ticks with the live price.
  // Intraday views use the live WS series unchanged.
  const historyBars = useHistoryBars(symbol, range);
  const displayCandles = useMemo(
    () => (range ? mergeLastBar(historyBars, lastPrice) : activeCandles),
    [range, historyBars, lastPrice, activeCandles]
  );
  const displayKey = range ? `${symbol}|${range}` : activeKey;

  // Bollinger + RSI computed from whatever series is shown (intraday or the chosen
  // historical range), shared by the chart overlay, the RSI pane, and the signal badge.
  const bands = useMemo(() => bollinger(displayCandles), [displayCandles]);
  const rsiData = useMemo(() => rsi(displayCandles), [displayCandles]);
  const signal = useMemo(() => evaluate(displayCandles, bands, rsiData), [displayCandles, bands, rsiData]);

  // Live equity: mark each held position to its streaming price between REST polls,
  // so Equity and Day P/L glide with prices instead of stepping every 3s. Cash is
  // constant between trades, so equity = cash + sum(position qty * live price).
  const liveEquity = useMemo(() => {
    if (!account) return 0;
    let posVal = 0;
    for (const p of positions) {
      const px = quotes[p.symbol]?.price ?? p.current_price ?? 0;
      posVal += p.qty * px;
    }
    return account.cash + posVal;
  }, [account, positions, quotes]);

  // Re-pull fills whenever holdings actually change (not on every quote tick) so we can
  // compute the true cost basis of the shares currently held (Alpaca's avg_entry_price
  // mis-blends same-day round-trips).
  const posSig = positions.map((p) => `${p.symbol}:${p.qty}`).sort().join(",");
  useEffect(() => {
    if (positions.length === 0) {
      setFills([]);
      return;
    }
    api.activities(90, 100).then(setFills).catch(() => {});
  }, [posSig]); // eslint-disable-line react-hooks/exhaustive-deps

  const basis = useMemo(() => basisBySymbol(positions, fills), [positions, fills]);

  // Day P&L in dollars = total unrealized profit/loss across held positions (live),
  // using the corrected per-share cost of the shares actually held.
  const dayPL = useMemo(() => {
    let pl = 0;
    for (const p of positions) {
      const px = quotes[p.symbol]?.price ?? p.current_price ?? 0;
      const entry = basis[p.symbol]?.avgEntry ?? p.avg_entry_price;
      pl += (px - entry) * p.qty;
    }
    return pl;
  }, [positions, quotes, basis]);

  // Held position of the currently-selected symbol (drives Sell-All, over-sell guard,
  // and the green "bought here" line).
  const heldPos = positions.find((p) => p.symbol === symbol);
  const heldQty = heldPos?.qty ?? 0;
  const entryPrice = basis[symbol]?.avgEntry ?? heldPos?.avg_entry_price ?? 0;
  const heldValue = heldPos ? heldPos.qty * (quotes[symbol]?.price ?? heldPos.current_price ?? 0) : 0;

  // Resting open orders for the selected symbol, drawn on the chart as colored lines:
  // green = take-profit (sell limit), red = stop-loss (sell stop), cyan = buy-the-dip limit,
  // blue = buy-stop. Only intraday views (historical ranges show their own static span).
  const orderLines = useMemo(() => {
    if (range) return [];
    const out: { id: string; price: number; color: string; label: string }[] = [];
    for (const o of orders) {
      if (o.symbol !== symbol) continue;
      const limit = parseFloat(o.limit_price) || 0;
      const stop = parseFloat(o.stop_price) || 0;
      const price = stop > 0 ? stop : limit;
      if (price <= 0) continue;
      const isStopType = stop > 0;
      let color = "#9aa4b2";
      let label = "order";
      // Dark green / wine red so the order lines stand apart from the bright candle colors.
      if (o.side === "sell" && isStopType) { color = "#a01f2e"; label = "stop loss"; }
      else if (o.side === "sell") { color = "#1f8a4c"; label = "take profit"; }
      else if (o.side === "buy" && isStopType) { color = "#5b8cff"; label = "buy stop"; }
      else { color = "#3fc7ff"; label = "buy limit"; }
      out.push({ id: o.id, price, color, label: `${label} $${price.toFixed(2)}` });
    }
    return out;
  }, [orders, symbol, range]);

  const lastCandles = useMemo(() => {
    const out: Record<string, Candle | undefined> = {};
    for (const s of cfg?.symbols ?? []) {
      out[s] = seriesRef.current.get(`${s}|${timeframe}`)?.slice(-1)[0];
    }
    return out;
  }, [cfg, timeframe, activeCandles]);

  // Seed left-panel prices AND reference prices from the server's engine snapshot on
  // load, so every row shows a price and a % change immediately — not just symbols whose
  // chart has been opened (which is the only way refPrices used to get populated).
  // Live WS quotes overwrite the prices within milliseconds during market hours.
  useEffect(() => {
    if (!cfg) return;
    let alive = true;
    api
      .quotesSnapshot()
      .then((snap) => {
        if (!alive) return;
        const now = Math.floor(Date.now() / 1000);
        for (const [sym, { ref }] of Object.entries(snap)) {
          if (ref > 0 && !refPrices.current[sym]) refPrices.current[sym] = ref;
        }
        setQuotes((q) => {
          const next = { ...q };
          for (const [sym, { price }] of Object.entries(snap)) {
            if (price > 0 && next[sym] === undefined) next[sym] = { symbol: sym, price, time: now };
          }
          return next;
        });
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [cfg]);

  async function confirmOrder() {
    if (!pending) return;
    setPlacing(true);
    try {
      const o = await api.placeOrder(pending.req);
      const sizeTxt = pending.req.notional ? `$${pending.req.notional}` : `${pending.req.qty} sh`;
      setToast(`✓ ${pending.req.side.toUpperCase()} ${pending.req.symbol} ${sizeTxt} — order ${o.status}`);
      setPending(null);
      setResetNonce((n) => n + 1); // clear the order-panel inputs
    } catch (e) {
      setToast(`Order failed: ${(e as Error).message}`);
    } finally {
      setPlacing(false);
      setTimeout(() => setToast(""), 5000);
    }
  }

  async function cancelOrder(id: string) {
    try {
      await api.cancelOrder(id);
    } catch (e) {
      setToast(`Cancel failed: ${(e as Error).message}`);
    }
  }

  async function removeSymbol(sym: string) {
    try {
      await api.removeExecSymbol(sym); // exec only — stays in the Watchlist
      const c = await api.config();
      setCfg(c);
      if (symbol === sym && c.symbols.length > 0) setSymbol(c.symbols[0]);
      setToast(`Removed ${sym} from Execution (still in Watchlist).`);
    } catch (e) {
      setToast(`Remove failed: ${(e as Error).message}`);
    }
    setTimeout(() => setToast(""), 5000);
  }

  async function removeFromBoth(sym: string) {
    try {
      await api.removeExecSymbol(sym, true); // remove from Execution AND Watchlist
      const c = await api.config();
      setCfg(c);
      if (symbol === sym && c.symbols.length > 0) setSymbol(c.symbols[0]);
      setToast(`Removed ${sym} from Execution + Watchlist.`);
    } catch (e) {
      setToast(`Remove failed: ${(e as Error).message}`);
    }
    setTimeout(() => setToast(""), 5000);
  }

  async function killSwitch() {
    if (!confirm("Cancel ALL open orders?")) return;
    try {
      await api.cancelAll();
      setToast("All open orders canceled.");
    } catch (e) {
      setToast(`Cancel-all failed: ${(e as Error).message}`);
    }
    setTimeout(() => setToast(""), 5000);
  }

  if (!cfg) {
    return <div className="boot">Connecting to Earth-Altra…</div>;
  }

  return (
    <div className="app">
      <Header
        mode={cfg.mode}
        feed={cfg.feed}
        status={status}
        account={account}
        liveEquity={liveEquity}
        dayPL={dayPL}
        sipEntitled={sipEntitled}
        onKillSwitch={killSwitch}
      />

      <div className="layout">
        <aside className="left">
          <Watchlist
            symbols={cfg.symbols}
            selected={symbol}
            onSelect={setSymbol}
            quotes={quotes}
            refPrices={refPrices.current}
            lastCandles={lastCandles}
            addedSymbols={cfg.added_symbols}
            onRemove={removeSymbol}
            onRemoveBoth={removeFromBoth}
          />
        </aside>

        <main className="center">
          <div className="chart-toolbar">
            <div className="chart-title selected">
              <span className="chart-sym">{symbol}</span>
              <span className="chart-last">{lastPrice > 0 ? `$${lastPrice.toFixed(2)}` : ""}</span>
              {rvol?.available && (
                <span
                  className={`rvol-badge ${rvol.value >= 1.5 ? "hot" : rvol.value < 0.7 ? "cold" : "norm"}`}
                  title="Relative volume: today's volume vs what's normal for this stock at this time of day. ~1.0 = normal, above 1.5 = unusually active (a real move), below 0.7 = quiet."
                >
                  RVOL {rvol.value.toFixed(2)}×
                </span>
              )}
              <span className="tz-note">times: {TZ_LABEL}</span>
            </div>
            <div className="toolbar-right">
              {showIndicators && <StrategyBadge result={signal} />}
              {!range && (
                <button
                  className={`ind-toggle ${drawMode ? "on" : ""}`}
                  onClick={() => {
                    setDrawMode((on) => !on);
                    setDraftPrice(null);
                  }}
                  title="Draw a price level on the chart to place an order"
                >
                  ✏ Draw order
                </button>
              )}
              <button
                className={`ind-toggle ${showIndicators ? "on" : ""}`}
                onClick={toggleIndicators}
                title="Show/hide Bollinger Bands + RSI"
              >
                Indicators
              </button>
              <RangeToggle view={view} onChange={setView} />
            </div>
          </div>
          <div className="chart-host">
            <Chart
              candles={displayCandles}
              seriesKey={displayKey}
              entryPrice={entryPrice}
              showIndicators={showIndicators}
              showRsiPane={showIndicators}
              bands={bands}
              rsiData={rsiData}
              history={!!range}
              drawMode={drawMode && !range}
              onPriceSelect={setDraftPrice}
              draftPrice={draftPrice}
              draftColor={draftType ? DRAW_MARKS[draftType].color : undefined}
              draftLabel={draftType ? DRAW_MARKS[draftType].label : undefined}
              orderLines={orderLines}
            />
            {drawMode && draftPrice == null && (
              <div className="draw-hint">Click a price level on the chart to place an order</div>
            )}
            {draftPrice != null && (
              <ChartOrderPopup
                symbol={symbol}
                price={draftPrice}
                lastPrice={lastPrice}
                heldQty={heldQty}
                heldValue={heldValue}
                fractionable={fractionable}
                maxNotional={cfg.max_order_notional}
                onTypeChange={setDraftType}
                onReview={(req, est) => {
                  setPending({ req, est });
                  setDraftPrice(null);
                  setDraftType(null);
                  setDrawMode(false);
                }}
                onClose={() => {
                  setDraftPrice(null);
                  setDraftType(null);
                }}
              />
            )}
          </div>
          <Positions positions={positions} orders={orders} quotes={quotes} fills={fills} basis={basis} onCancel={cancelOrder} />
          {symbol && <NewsPanel symbol={symbol} />}
        </main>

        <aside className="right">
          <OrderPanel
            symbol={symbol}
            lastPrice={lastPrice}
            fractionable={fractionable}
            maxNotional={cfg.max_order_notional}
            heldQty={heldQty}
            heldValue={heldValue}
            resetNonce={resetNonce}
            onReview={(req, est) => setPending({ req, est })}
          />
        </aside>
      </div>

      {pending && (
        <ConfirmModal
          req={pending.req}
          estCost={pending.est}
          mode={cfg.mode}
          currentPrice={lastPrice}
          busy={placing}
          onConfirm={confirmOrder}
          onCancel={() => setPending(null)}
        />
      )}

      {toast && <div className="toast">{toast}</div>}
    </div>
  );
}
