package api

import (
	"net/http"
	"strings"

	"live-optimus/backend/internal/quant"
)

// quantReport returns the dip-driven AI pipeline's state for the Paper·Claude page (budget,
// positions, realized P&L, Agent-3 exit attribution, dip-agent scorecard, the roster of
// agents + their models, and the latest review). Read-only.
func (s *Server) quantReport(w http.ResponseWriter, r *http.Request) {
	if s.Quant == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	rep := s.Quant.Report()
	rep.Agents = s.quantAgents()
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "report": rep})
}

// dipRiseReport returns the Dip+Rise desk's full page state: desk mode, its account's
// positions/trades, armed rise-watch dips, the dip scorecard, and the recent decision
// timeline (dips → Agent 2 verdicts → arms/triggers → funding → outcomes). Read-only.
func (s *Server) dipRiseReport(w http.ResponseWriter, r *http.Request) {
	if s.Quant == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.Quant.DipRiseReport())
}

// ridpReport returns the RIDP two-strategy paper desk state (RIDER + DIPPER: open
// positions, budget/allocation, per-strategy stats, dip setups being watched). Read-only.
func (s *Server) ridpReport(w http.ResponseWriter, r *http.Request) {
	if s.Ridp == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.Ridp())
}

// rbtReport returns the RBT desk state for the RBT page.
func (s *Server) rbtReport(w http.ResponseWriter, r *http.Request) {
	if s.Rbt == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "report": s.Rbt()})
}

// sndkReport returns the SNDK desk state for the SNDK page.
func (s *Server) sndkReport(w http.ResponseWriter, r *http.Request) {
	if s.Sndk == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "report": s.Sndk()})
}

// breadcrumbsReport returns the Breadcrumbs generalized-scalper desk state for its page.
func (s *Server) breadcrumbsReport(w http.ResponseWriter, r *http.Request) {
	if s.Breadcrumbs == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "report": s.Breadcrumbs()})
}

// surgerReport returns the SURGER v2 lab state (three variants on the dip+rise account).
func (s *Server) surgerReport(w http.ResponseWriter, r *http.Request) {
	if s.Surger == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, s.Surger())
}

// quantAgents lists every agent on the desk with its ACTUAL configured model and whether
// it's live — the single source of truth is the config, so the page can't drift out of
// sync with what's really running (which is how the stale "Opus" label happened).
func (s *Server) quantAgents() []quant.AgentInfo {
	c := s.Cfg
	llm := strings.TrimSpace(c.AnthropicAPIKey) != ""
	dipArmed := c.PaperDipKey != "" && c.PaperDipSecret != ""
	return []quant.AgentInfo{
		{Name: "Strategist", Model: c.QuantStrategistModel, Role: "pre-market posture + budget (both desks)", Live: llm && c.QuantStrategist},
		{Name: "Signal engine", Model: "deterministic (Go)", Role: "6 strategies detect setups (signal desk)", Live: true},
		{Name: "ML entry gate", Model: "LightGBM (6 models)", Role: "score expected R, reject weak setups", Live: c.QuantClfGate},
		{Name: "Signal judge", Model: c.QuantJudgeModel, Role: "red-flag veto + conviction (signal desk)", Live: llm && c.QuantSignalsLive},
		{Name: "Agent 2 · Entry", Model: c.QuantEntryModel, Role: "buy/no-buy on dips (dip+rise desk)", Live: llm && c.QuantLive && dipArmed},
		{Name: "Rise watcher", Model: "deterministic (Go)", Role: "confirmed post-dip bounces (dip+rise desk)", Live: c.QuantRiseLive && dipArmed},
		{Name: "Agent 3 · Exit", Model: c.QuantExitModel, Role: "trailing stop + discretionary exits (both desks)", Live: llm},
		{Name: "Agent 4 · Sentiment", Model: c.OllamaModel + " (local)", Role: "advisory sentiment", Live: c.QuantSentiment},
		{Name: "Reviewer", Model: c.QuantReviewModel, Role: "daily report card", Live: llm},
	}
}
