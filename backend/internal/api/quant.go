package api

import (
	"net/http"
)

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

// regimeReport returns the shadow regime detector's prediction/outcome journal state.
func (s *Server) regimeReport(w http.ResponseWriter, r *http.Request) {
	if s.Regime == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "report": s.Regime()})
}

func (s *Server) moverWatchReport(w http.ResponseWriter, r *http.Request) {
	if s.MoverWatch == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "report": s.MoverWatch()})
}
