package control

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	defaultAnalyticsTop   = 8
	maxAnalyticsTop       = 50
	maxHourBucketSpan     = 31 * 24 * time.Hour
	hourBucketMaxDefault  = 48 * time.Hour
	dimensionsTop         = 50
	defaultAnalyticsRange = "7d"
	defaultDimsRange      = "30d"
	defaultAnalyticsGroup = "host"
	filterPrefix          = "filter."
)

// analyticsRanges are the accepted relative ranges (to = now).
var analyticsRanges = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
}

// analyticsErrPrefix marks ledger validation errors (mapped to 400).
const analyticsErrPrefix = "analytics:"

// dimensionValue is one distinct value of a dimension in the dimensions doc.
type dimensionValue struct {
	Key      string `json:"key"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// dimensionsDoc is the GET /control/v1/analytics/dimensions response.
type dimensionsDoc struct {
	SchemaVersion int                         `json:"schema_version"`
	From          time.Time                   `json:"from"`
	To            time.Time                   `json:"to"`
	Dimensions    map[string][]dimensionValue `json:"dimensions"`
}

func (s *Server) analyticsLedger(w http.ResponseWriter) (core.AnalyticsLedger, bool) {
	al, ok := s.deps.Ledger.(core.AnalyticsLedger)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable")
		return nil, false
	}
	return al, true
}

func (s *Server) analytics(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	q, err := s.parseAnalyticsQuery(params, defaultAnalyticsRange)
	if err == nil {
		err = parseAnalyticsShape(params, &q)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	al, ok := s.analyticsLedger(w)
	if !ok {
		return
	}
	res, err := al.Analytics(r.Context(), q)
	if err != nil {
		s.analyticsFailed(w, err, q.Group)
		return
	}
	writeJSON(w, http.StatusOK, normalizeResult(res))
}

func (s *Server) analyticsDimensions(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	q, err := s.parseAnalyticsQuery(params, defaultDimsRange)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, k := range []string{"bucket", "group", "top"} {
		if params.Has(k) {
			writeError(w, http.StatusBadRequest, k+": not accepted by analytics/dimensions")
			return
		}
	}
	al, ok := s.analyticsLedger(w)
	if !ok {
		return
	}
	q.Bucket, q.TopN = "day", dimensionsTop
	doc := dimensionsDoc{
		SchemaVersion: SchemaVersion, From: q.From.UTC(), To: q.To.UTC(),
		Dimensions: make(map[string][]dimensionValue, len(core.AnalyticsDimensions)),
	}
	for _, dim := range core.AnalyticsDimensions {
		q.Group = dim
		res, err := al.Analytics(r.Context(), q)
		if err != nil {
			s.analyticsFailed(w, err, dim)
			return
		}
		vals := make([]dimensionValue, 0, min(len(res.Breakdown), dimensionsTop))
		for _, row := range res.Breakdown[:min(len(res.Breakdown), dimensionsTop)] {
			tokens := row.InputTokens + row.OutputTokens
			if tokens < row.InputTokens || tokens < row.OutputTokens { // saturate on overflow
				tokens = math.MaxInt64
			}
			vals = append(vals, dimensionValue{Key: row.Key, Requests: row.Requests, Tokens: tokens})
		}
		doc.Dimensions[dim] = vals
	}
	writeJSON(w, http.StatusOK, doc)
}

// analyticsFailed maps ledger errors: "analytics: ..." are validation
// failures (400, message passed through); anything else is a 500.
func (s *Server) analyticsFailed(w http.ResponseWriter, err error, group string) {
	if msg := err.Error(); strings.HasPrefix(msg, analyticsErrPrefix) {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	slog.Warn("control: analytics failed", "group", group, "err", err)
	writeError(w, http.StatusInternalServerError, "analytics failed")
}

// parseAnalyticsQuery parses the time range (range, from, to) and filters
// shared by both analytics endpoints.
func (s *Server) parseAnalyticsQuery(params url.Values, defRange string) (core.AnalyticsQuery, error) {
	var q core.AnalyticsQuery
	for k, v := range params {
		if len(v) > 1 && (k == "range" || k == "from" || k == "to" || k == "bucket" || k == "group" || k == "top" || strings.HasPrefix(k, filterPrefix)) {
			return q, fmt.Errorf("%s: given more than once", k)
		}
	}
	rng := defRange
	if params.Has("range") {
		rng = params.Get("range")
	}
	span, ok := analyticsRanges[rng]
	if !ok {
		return q, errors.New("range: must be one of 24h, 7d, 30d, 90d")
	}
	q.To = s.deps.Clock.Now()
	if params.Has("to") {
		t, err := time.Parse(time.RFC3339, params.Get("to"))
		if err != nil {
			return q, errors.New("to: expected an RFC 3339 timestamp")
		}
		q.To = t
	}
	q.From = q.To.Add(-span)
	if params.Has("from") {
		t, err := time.Parse(time.RFC3339, params.Get("from"))
		if err != nil {
			return q, errors.New("from: expected an RFC 3339 timestamp")
		}
		q.From = t
	}
	if !q.From.Before(q.To) {
		return q, errors.New("from must be before to")
	}
	if q.To.Sub(q.From) > maxSince {
		return q, errors.New("time span must be at most 400 days")
	}

	q.Filters = map[string]string{}
	for k, v := range params {
		dim, ok := strings.CutPrefix(k, filterPrefix)
		if !ok {
			continue
		}
		if !slices.Contains(core.AnalyticsDimensions, dim) {
			return q, fmt.Errorf("%s: unknown dimension (want one of %s)", k, strings.Join(core.AnalyticsDimensions, ", "))
		}
		q.Filters[dim] = v[0]
	}
	return q, nil
}

// parseAnalyticsShape parses bucket, group and top for GET /analytics.
func parseAnalyticsShape(params url.Values, q *core.AnalyticsQuery) error {
	span := q.To.Sub(q.From)
	q.Bucket = params.Get("bucket")
	switch q.Bucket {
	case "":
		q.Bucket = "hour"
		if span > hourBucketMaxDefault {
			q.Bucket = "day"
		}
	case "hour":
		if span > maxHourBucketSpan {
			return errors.New("bucket: hour buckets allow at most 31 days")
		}
	case "day":
	default:
		return errors.New("bucket: must be hour or day")
	}

	q.Group = params.Get("group")
	if q.Group == "" {
		q.Group = defaultAnalyticsGroup
	}
	if !slices.Contains(core.AnalyticsDimensions, q.Group) {
		return fmt.Errorf("group: must be one of %s", strings.Join(core.AnalyticsDimensions, ", "))
	}

	q.TopN = defaultAnalyticsTop
	if params.Has("top") {
		n, err := strconv.Atoi(params.Get("top"))
		if err != nil || n < 1 || n > maxAnalyticsTop {
			return fmt.Errorf("top: must be an integer between 1 and %d", maxAnalyticsTop)
		}
		q.TopN = n
	}
	return nil
}

// normalizeResult fills defaults so the JSON never has null arrays/maps.
func normalizeResult(res core.AnalyticsResult) core.AnalyticsResult {
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
		res.Breakdown = []core.UsageRow{}
	}
	if res.Series == nil {
		res.Series = []core.AnalyticsSeries{}
	}
	for i := range res.Series {
		if res.Series[i].Points == nil {
			res.Series[i].Points = []core.UsageRow{}
		}
	}
	return res
}
