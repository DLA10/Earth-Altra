package api

import "net/http"

// quantReport returns the dip-driven AI pipeline's state for the Paper·Claude page (budget,
// positions, realized P&L, Agent-3 exit attribution, latest review). Read-only.
func (s *Server) quantReport(w http.ResponseWriter, r *http.Request) {
	if s.Quant == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	rep := s.Quant.Report()
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "report": rep})
}
