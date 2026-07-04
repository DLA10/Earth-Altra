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

// quantAgents lists every agent on the desk with its ACTUAL configured model and whether
// it's live — the single source of truth is the config, so the page can't drift out of
// sync with what's really running (which is how the stale "Opus" label happened).
func (s *Server) quantAgents() []quant.AgentInfo {
	c := s.Cfg
	llm := strings.TrimSpace(c.AnthropicAPIKey) != ""
	return []quant.AgentInfo{
		{Name: "Strategist", Model: c.QuantStrategistModel, Role: "pre-market posture + budget", Live: llm && c.QuantStrategist},
		{Name: "Signal engine", Model: "deterministic (Go)", Role: "6 strategies detect setups", Live: true},
		{Name: "ML entry gate", Model: "LightGBM (6 models)", Role: "score expected R, reject weak setups", Live: c.QuantClfGate},
		{Name: "Signal judge", Model: c.QuantJudgeModel, Role: "red-flag veto + conviction (signal pipeline)", Live: llm && c.QuantSignalsLive},
		{Name: "Agent 2 · Entry", Model: c.QuantEntryModel, Role: "buy/no-buy on dips (dip pipeline)", Live: llm && c.QuantLive},
		{Name: "Agent 3 · Exit", Model: c.QuantExitModel, Role: "trailing stop + discretionary exits (both pipelines)", Live: llm},
		{Name: "Agent 4 · Sentiment", Model: c.OllamaModel + " (local)", Role: "advisory sentiment", Live: true},
		{Name: "Reviewer", Model: c.QuantReviewModel, Role: "daily report card", Live: llm},
	}
}
