package quant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"live-optimus/backend/internal/candles"
)

// TestRehydrateAdoptsSurvivingPosition: after a restart, an open paper position with a
// working trailing stop must be re-registered, re-funded in the allocator, and resume
// management — with the existing stop adopted, not duplicated.
func TestRehydrateAdoptsSurvivingPosition(t *testing.T) {
	var stopOrdersPlaced int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/positions":
			w.Write([]byte(`[{"symbol":"PLTR","qty":"3","avg_entry_price":"130.60"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/orders":
			w.Write([]byte(`[
			 {"id":"o1","client_order_id":"QuantDip__PLTR__entry__1","symbol":"PLTR","side":"buy","type":"market","qty":"3","filled_qty":"3","filled_avg_price":"130.60","status":"filled","filled_at":"2026-07-02T17:07:03Z","submitted_at":"2026-07-02T17:07:02Z"},
			 {"id":"o2","client_order_id":"QuantDip__PLTR__exit__Trail_Stop__2","symbol":"PLTR","side":"sell","type":"trailing_stop","qty":"3","stop_price":"128.64","filled_qty":"0","filled_avg_price":"","status":"new","submitted_at":"2026-07-02T17:07:04Z"}
			]`))
		case r.Method == http.MethodPost && r.URL.Path == "/orders":
			stopOrdersPlaced++
			w.Write([]byte(`{"id":"new-stop"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/positions/PLTR":
			w.Write([]byte(`{"qty":"3"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	alloc := NewAllocator()
	eng := NewEngine(nil, alloc, nil, nil, candles.NewEngine([]string{"PLTR"}, 10), nil, time.UTC)
	broker := NewBroker(srv.URL, "k", "s")
	mgr := NewManager(eng, alloc, broker, nil, 1.5, 0)
	eng.SetExecution(broker, mgr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the resumed manage loop

	if n := mgr.Rehydrate(ctx); n != 1 {
		t.Fatalf("expected 1 rehydrated position, got %d", n)
	}
	if !alloc.Held("PLTR") {
		t.Fatal("allocator must be re-funded with the surviving position")
	}
	if got := alloc.Snapshot().Deployed; got < 391 || got > 393 {
		t.Fatalf("deployed capital should be ~$391.80, got %.2f", got)
	}
	syms := mgr.OpenSymbols()
	if len(syms) != 1 || syms[0] != "PLTR" {
		t.Fatalf("manager must track PLTR, got %v", syms)
	}
	if stopOrdersPlaced != 0 {
		t.Fatalf("existing trailing stop must be ADOPTED, not duplicated (placed %d new)", stopOrdersPlaced)
	}
	// Second call must be a no-op (idempotent).
	if n := mgr.Rehydrate(ctx); n != 0 {
		t.Fatalf("rehydrate must be idempotent, adopted %d again", n)
	}
}

// TestRehydrateSkipsSiblingDeskPositions: a position whose newest filled buy carries a
// sibling desk's coid prefix (ridp_/rbt_/sndk_ — shared paper account) must NOT be
// adopted: no stop cancel, no fresh stop, no allocator funding. This is the 2026-07-13/14
// incident guard (Rehydrate adopted RIDP's reverter positions and Agent 3 sold them).
func TestRehydrateSkipsSiblingDeskPositions(t *testing.T) {
	var ordersPlaced, cancels int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/positions":
			w.Write([]byte(`[{"symbol":"ANET","qty":"8","avg_entry_price":"184.38"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/orders":
			w.Write([]byte(`[
			 {"id":"r1","client_order_id":"ridp_reverter__ANET__entry__1","symbol":"ANET","side":"buy","type":"market","qty":"8","filled_qty":"8","filled_avg_price":"184.38","status":"filled","filled_at":"2026-07-13T13:45:00Z","submitted_at":"2026-07-13T13:44:59Z"},
			 {"id":"r2","client_order_id":"ridp_reverter__ANET__stop__2","symbol":"ANET","side":"sell","type":"stop","qty":"8","stop_price":"180.00","filled_qty":"0","filled_avg_price":"","status":"new","submitted_at":"2026-07-13T13:45:01Z"}
			]`))
		case r.Method == http.MethodPost && r.URL.Path == "/orders":
			ordersPlaced++
			w.Write([]byte(`{"id":"should-not-happen"}`))
		case r.Method == http.MethodDelete:
			cancels++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	alloc := NewAllocator()
	eng := NewEngine(nil, alloc, nil, nil, candles.NewEngine([]string{"ANET"}, 10), nil, time.UTC)
	broker := NewBroker(srv.URL, "k", "s")
	mgr := NewManager(eng, alloc, broker, nil, 1.5, 0)
	eng.SetExecution(broker, mgr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if n := mgr.Rehydrate(ctx); n != 0 {
		t.Fatalf("sibling desk's position must not be adopted, got %d", n)
	}
	if alloc.Held("ANET") {
		t.Fatal("allocator must not be funded with a sibling desk's position")
	}
	if ordersPlaced != 0 || cancels != 0 {
		t.Fatalf("sibling desk's protective stop must be left alone (placed %d, canceled %d)", ordersPlaced, cancels)
	}
}

// TestRehydratePlacesStopWhenMissing: a surviving position with NO working stop must get
// a fresh trailing stop immediately (the never-unprotected invariant).
func TestRehydratePlacesStopWhenMissing(t *testing.T) {
	var stopOrdersPlaced int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/positions":
			w.Write([]byte(`[{"symbol":"AMD","qty":"2","avg_entry_price":"500.00"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/orders":
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/orders":
			stopOrdersPlaced++
			w.Write([]byte(`{"id":"fresh-stop"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	alloc := NewAllocator()
	eng := NewEngine(nil, alloc, nil, nil, candles.NewEngine([]string{"AMD"}, 10), nil, time.UTC)
	broker := NewBroker(srv.URL, "k", "s")
	mgr := NewManager(eng, alloc, broker, nil, 1.5, 0)
	eng.SetExecution(broker, mgr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if n := mgr.Rehydrate(ctx); n != 1 {
		t.Fatalf("expected 1 rehydrated position, got %d", n)
	}
	if stopOrdersPlaced != 1 {
		t.Fatalf("an unprotected survivor must get exactly one fresh stop, placed %d", stopOrdersPlaced)
	}
}
