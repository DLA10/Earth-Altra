// Package hub fans out market-data and account events to connected browser clients
// over WebSocket. Each client subscribes to a single symbol at a time; all clients
// receive quote updates for their symbol and a periodic account/positions snapshot.
package hub

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Message is the envelope sent to browser clients.
type Message struct {
	Type string      `json:"type"` // "candle" | "quote" | "snapshot" | "account" | "positions" | "orders" | "error"
	Data interface{} `json:"data"`
}

// Quote is a lightweight last-price update for the watchlist.
type Quote struct {
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	Time   int64   `json:"time"`
}

type client struct {
	id        int64
	conn      *websocket.Conn
	send      chan []byte
	symbol    string // currently-subscribed symbol for candle stream
	timeframe int    // currently-subscribed timeframe (minutes) for candle stream
	scanSub   bool   // subscribed to DECEPTICON scan updates
	mu        sync.RWMutex
}

func (c *client) scanSubscribed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.scanSub
}

func (c *client) setScanSub(v bool) {
	c.mu.Lock()
	c.scanSub = v
	c.mu.Unlock()
}

func (c *client) currentSub() (string, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.symbol, c.timeframe
}

func (c *client) setSub(symbol string, timeframe int) {
	c.mu.Lock()
	c.symbol = symbol
	c.timeframe = timeframe
	c.mu.Unlock()
}

// Hub tracks connected clients and broadcasts messages.
type Hub struct {
	mu      sync.RWMutex
	clients map[int64]*client
	nextID  int64

	// SnapshotFn returns the candle history for a (symbol, timeframe) when a client
	// subscribes. Set by the server.
	SnapshotFn func(symbol string, timeframe int) interface{}

	// EnsureLiveFn makes a symbol live (backfill + subscribe its trades) if it isn't
	// already, so a client can subscribe to ANY symbol (e.g. a DECEPTICON market mover)
	// and get a real-time chart. Called synchronously before the snapshot so the
	// snapshot already carries the backfilled history. Additive only — never tears a
	// subscription down. Set by the server; nil = no on-demand activation.
	EnsureLiveFn func(symbol string)

	// Throttle high-frequency quote/candle broadcasts per symbol so the SIP firehose
	// can't overrun a client's send buffer (which previously caused dropped snapshots
	// and blank charts after a long session).
	rateMu   sync.Mutex
	lastSent map[string]time.Time
}

// New creates a Hub.
func New() *Hub {
	return &Hub{clients: map[int64]*client{}, lastSent: map[string]time.Time{}}
}

// allow rate-limits a keyed broadcast to at most once per `every`.
func (h *Hub) allow(key string, every time.Duration) bool {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	now := time.Now()
	if last, ok := h.lastSent[key]; ok && now.Sub(last) < every {
		return false
	}
	h.lastSent[key] = now
	return true
}

// BroadcastCandle sends a candle update only to clients subscribed to that symbol AND
// timeframe. Throttled per (symbol, timeframe) — one trade emits an update per timeframe,
// so a shared per-symbol key would let map-iteration order pick which timeframe got
// through, starving the others at random. The forming bar still updates several times a
// second; the authoritative 1-minute bar corrects any skipped intermediate tick.
func (h *Hub) BroadcastCandle(symbol string, timeframe int, msg interface{}) {
	if !h.allow("c:"+symbol+"|"+strconv.Itoa(timeframe), 120*time.Millisecond) {
		return
	}
	payload, err := json.Marshal(Message{Type: "candle", Data: msg})
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if sym, tf := c.currentSub(); sym == symbol && tf == timeframe {
			h.trySend(c, payload)
		}
	}
}

// BroadcastQuote sends a last-price quote to all clients (drives the watchlist).
// Throttled per symbol so dozens of streamed symbols can't flood every client.
func (h *Hub) BroadcastQuote(q Quote) {
	if !h.allow("q:"+q.Symbol, 150*time.Millisecond) {
		return
	}
	payload, err := json.Marshal(Message{Type: "quote", Data: q})
	if err != nil {
		return
	}
	h.broadcastAll(payload)
}

// BroadcastTyped sends an arbitrary typed message to all clients.
func (h *Hub) BroadcastTyped(t string, data interface{}) {
	payload, err := json.Marshal(Message{Type: t, Data: data})
	if err != nil {
		return
	}
	h.broadcastAll(payload)
}

// ScanSubscriberCount returns how many clients are subscribed to scan updates.
func (h *Hub) ScanSubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, c := range h.clients {
		if c.scanSubscribed() {
			n++
		}
	}
	return n
}

// BroadcastScan sends a DECEPTICON scan snapshot only to scan-subscribed clients.
func (h *Hub) BroadcastScan(data interface{}) {
	payload, err := json.Marshal(Message{Type: "scan", Data: data})
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.scanSubscribed() {
			h.trySend(c, payload)
		}
	}
}

func (h *Hub) broadcastAll(payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		h.trySend(c, payload)
	}
}

// trySend performs a non-blocking send; if the client's buffer is full we drop the
// message rather than stall the whole hub (the next update supersedes it anyway).
func (h *Hub) trySend(c *client, payload []byte) {
	select {
	case c.send <- payload:
	default:
		// Slow client; drop this frame.
	}
}

// clientRequest is what the browser sends us.
type clientRequest struct {
	Action    string `json:"action"` // "subscribe" | "scan_subscribe" | "scan_unsubscribe"
	Symbol    string `json:"symbol"`
	Timeframe int    `json:"timeframe"`
}

// Serve handles a single WebSocket connection lifecycle.
func (h *Hub) Serve(ctx context.Context, conn *websocket.Conn) {
	conn.SetReadLimit(1 << 20)

	h.mu.Lock()
	h.nextID++
	c := &client{id: h.nextID, conn: conn, send: make(chan []byte, 1024)}
	h.clients[c.id] = c
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, c.id)
		h.mu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "bye")
	}()

	writerDone := make(chan struct{})
	go h.writePump(ctx, c, writerDone)

	// Read pump: handle subscribe requests.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var req clientRequest
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		switch req.Action {
		case "scan_subscribe":
			c.setScanSub(true)
			continue
		case "scan_unsubscribe":
			c.setScanSub(false)
			continue
		}
		// Normalize: the engine and all broadcasts key symbols in upper case.
		req.Symbol = strings.ToUpper(strings.TrimSpace(req.Symbol))
		if req.Action == "subscribe" && req.Symbol != "" {
			tf := req.Timeframe
			if tf == 0 {
				tf = 1
			}
			c.setSub(req.Symbol, tf)
			// Make the symbol live (backfill + trade subscription) if it isn't already,
			// so previewing any symbol streams in real time. Synchronous so the snapshot
			// below carries the backfilled history; a no-op for already-live symbols.
			if h.EnsureLiveFn != nil {
				h.EnsureLiveFn(req.Symbol)
			}
			if h.SnapshotFn != nil {
				snap := h.SnapshotFn(req.Symbol, tf)
				if payload, err := json.Marshal(Message{Type: "snapshot", Data: map[string]interface{}{
					"symbol":    req.Symbol,
					"timeframe": tf,
					"candles":   snap,
				}}); err == nil {
					// The snapshot is the one-time payload that populates the chart on
					// switch — it must NOT be dropped under buffer pressure, so send it
					// reliably (blocking with a timeout) instead of best-effort.
					select {
					case c.send <- payload:
					case <-time.After(3 * time.Second):
					case <-ctx.Done():
					}
				}
			}
		}
	}
	close(writerDone)
}

func (h *Hub) writePump(ctx context.Context, c *client, done chan struct{}) {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case msg := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

var _ = log.Println
