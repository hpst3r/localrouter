package control

import (
	"net/http"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// analyticsPerformance serves GET /control/v1/analytics/performance: the
// same query parameters, defaults and errors as GET /control/v1/analytics,
// answered by core.PerfLedger (SPEC "Performance metrics").
func (s *Server) analyticsPerformance(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	q, err := s.parseAnalyticsQuery(params, defaultAnalyticsRange)
	if err == nil {
		err = parseAnalyticsShape(params, &q)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	pl, ok := s.deps.Ledger.(core.PerfLedger)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "performance analytics unavailable")
		return
	}
	res, err := pl.Performance(r.Context(), q)
	if err != nil {
		s.analyticsFailed(w, err, q.Group)
		return
	}
	writeJSON(w, http.StatusOK, normalizePerf(res))
}

// normalizePerf fills defaults so the JSON never has null arrays/maps.
func normalizePerf(res core.PerfResult) core.PerfResult {
	if res.SchemaVersion == 0 {
		res.SchemaVersion = SchemaVersion
	}
	if res.BucketStarts == nil {
		res.BucketStarts = []time.Time{}
	}
	if res.Filters == nil {
		res.Filters = map[string]string{}
	}
	if res.Breakdown == nil {
		res.Breakdown = []core.PerfRow{}
	}
	if res.Series == nil {
		res.Series = []core.PerfSeries{}
	}
	for i := range res.Series {
		if res.Series[i].Points == nil {
			res.Series[i].Points = []core.PerfRow{}
		}
	}
	return res
}
