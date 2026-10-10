package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/ferro-labs/ai-gateway/internal/admin/model"
	"github.com/ferro-labs/ai-gateway/internal/requestlog"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

func (h *Handlers) listLogs(w http.ResponseWriter, r *http.Request) {
	if h.Logs == nil {
		writeError(w, http.StatusNotImplemented, "request log storage is not enabled", "not_implemented_error", "not_implemented")
		return
	}

	limit, ok := parseLimit(w, r, defaultLogsLimit, maxLogsLimit)
	if !ok {
		return
	}

	offset, ok := parseOffset(w, r)
	if !ok {
		return
	}

	since, ok := parseSince(w, r)
	if !ok {
		return
	}

	stage := r.URL.Query().Get("stage")
	query := requestlog.Query{
		Limit:    limit,
		Offset:   offset,
		Model:    r.URL.Query().Get("model"),
		Provider: r.URL.Query().Get("provider"),
		APIKeyID: parseAPIKeyID(r),
		Since:    since,
	}
	switch stage {
	case "":
		// One row per request by default. The logger writes one row per plugin
		// stage, so the unfiltered list interleaved each request with a
		// provider-less, zero-token copy of itself and every caller reading it
		// as "the requests" listed each one twice.
		query.Stages = requestlog.TerminalStages()
	case stageAll:
		// Every logged row, which is what this route returned before the
		// default narrowed. Kept so a caller that genuinely wants the raw
		// event stream — following one request through its stages — has a way
		// to ask, rather than only being able to name one stage at a time.
	default:
		query.Stage = stage
	}

	result, err := h.Logs.List(r.Context(), query)
	if err != nil {
		writeLogStoreError(w, err, "failed to list request logs")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": result.Data,
		"summary": map[string]any{
			"total_entries":    result.Total,
			"returned_entries": len(result.Data),
		},
		"filters": map[string]any{
			"limit":  limit,
			"offset": offset,
			// Echoed as asked for, including the empty default, so a client can
			// tell "I filtered to after_request" from "the gateway defaulted to
			// one row per request" — which return overlapping but different sets.
			"stage":  stage,
			"stages": query.Stages,
			"model":  query.Model,
			// Echoed as asked for rather than as resolved, so `none` comes back
			// as `none`: the resolved form is the empty string, which is also
			// how "no filter" is spelled here.
			"api_key_id": r.URL.Query().Get("api_key_id"),
			"provider":   query.Provider,
			"since":      r.URL.Query().Get("since"),
		},
	})
}

func (h *Handlers) deleteLogs(w http.ResponseWriter, r *http.Request) {
	if h.LogAdmin == nil {
		writeError(w, http.StatusNotImplemented, "request log storage is not enabled", "not_implemented_error", "not_implemented")
		return
	}

	beforeRaw := r.URL.Query().Get("before")
	if beforeRaw == "" {
		writeError(w, http.StatusBadRequest, "before is required and must be RFC3339 format", "invalid_request_error", "invalid_request")
		return
	}

	before, err := time.Parse(time.RFC3339, beforeRaw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid before: must be RFC3339 format", "invalid_request_error", "invalid_request")
		return
	}

	deleted, err := h.LogAdmin.Delete(r.Context(), requestlog.MaintenanceQuery{
		Before:   &before,
		Stage:    stageFilter(r),
		Model:    r.URL.Query().Get("model"),
		Provider: r.URL.Query().Get("provider"),
	})
	if err != nil {
		// A purge is audited, so one the store refused is too, exactly as a
		// failed key or config mutation is: a trail holding only the purges
		// that succeeded cannot say whether anyone tried.
		h.recordAudit(r, "logs.purge", "*", model.AuditError, "before", beforeRaw, "error", err.Error())
		writeLogStoreError(w, err, "failed to delete request logs")
		return
	}

	// A purge is destructive and irreversible, so it is audited like a
	// credential change. The count and cutoff are recorded because "who deleted
	// the logs" is only half the question; "how much, and back to when" is the
	// rest.
	h.recordAudit(r, "logs.purge", "*", model.AuditOK, "deleted", deleted, "before", beforeRaw)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"deleted": deleted,
		"filters": map[string]any{
			"before":   beforeRaw,
			"stage":    r.URL.Query().Get("stage"),
			"model":    r.URL.Query().Get("model"),
			"provider": r.URL.Query().Get("provider"),
		},
	})
}

func (h *Handlers) logsStats(w http.ResponseWriter, r *http.Request) {
	if h.Logs == nil {
		writeError(w, http.StatusNotImplemented, "request log storage is not enabled", "not_implemented_error", "not_implemented")
		return
	}

	limit, ok := parseLimit(w, r, 0, maxLogsStatsLimit)
	if !ok {
		return
	}

	since, ok := parseSince(w, r)
	if !ok {
		return
	}

	buckets, ok := parseBoundedQueryInt(w, r, "buckets", 0, maxLogsStatsBuckets)
	if !ok {
		return
	}
	// A series divides the window from since to now, so without since there is
	// no window and the store computes none. Answering 200 echoed the bucket
	// count beside an empty series marked not truncated, which reads as a
	// window that held no traffic.
	if buckets > 0 && since == nil {
		writeError(w, http.StatusBadRequest, "buckets requires since: a series divides the window from since to now", "invalid_request_error", "invalid_request")
		return
	}

	query := requestlog.Query{
		Stage:         stageFilter(r),
		Model:         r.URL.Query().Get("model"),
		Provider:      r.URL.Query().Get("provider"),
		Since:         since,
		SeriesBuckets: buckets,
		TopErrors:     logsStatsTopErrors,
	}

	stats, err := h.Logs.Stats(r.Context(), query)
	if err != nil {
		writeLogStoreError(w, err, "failed to compute request log stats")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"summary": map[string]any{
			"total_entries": stats.TotalEntries,
			"error_entries": stats.ErrorEntries,
			"total_tokens":  stats.TotalTokens,
			// Split out because the two halves are not interchangeable: output
			// tokens are the expensive ones, so a total alone hides the thing
			// that actually moves an AI gateway's bill.
			"prompt_tokens":     stats.PromptTokens,
			"completion_tokens": stats.CompletionTokens,
			// A floor on spend, not a total: requests whose model the catalog
			// does not price contribute nothing, and unpriced_requests says how
			// many. Reporting the floor alone would read as the whole bill.
			"cost_usd":          stats.TotalCostUSD,
			"unpriced_requests": stats.UnpricedRequests,
		},
		"latency_ms":  encodePercentiles(stats.LatencyMs),
		"ttft_ms":     encodePercentiles(stats.TTFTMs),
		"by_stage":    encodeDimension(stats.ByStage, 0),
		"by_provider": encodeDimension(stats.ByProvider, limit),
		"by_model":    encodeDimension(stats.ByModel, limit),
		"top_errors":  encodeErrorGroups(stats.TopErrors),
		"series":      encodeSeries(stats),
		"filters": map[string]any{
			"limit":    limit,
			"buckets":  buckets,
			"stage":    r.URL.Query().Get("stage"),
			"model":    query.Model,
			"provider": query.Provider,
			"since":    r.URL.Query().Get("since"),
		},
	})
}

// writeLogStoreError answers a request-log operation the store could not
// complete with a 500 naming the operation. The store's error is logged and
// never written, as the key and config stores' are: it can quote the database's
// host, port or user. Writing neither left the operator told only that the
// operation failed, with the reason recorded nowhere.
func writeLogStoreError(w http.ResponseWriter, err error, message string) {
	logger.Default().Error("admin request log store operation failed", "error", err)
	writeError(w, http.StatusInternalServerError, message, "server_error", "internal_error")
}

// stageFilter is the single stage a purge or a stats query narrows to, or ""
// for none.
//
// stageAll is how GET /admin/logs is asked for every logged row, so it means
// "every stage" on the two routes beside it too. Passed through as a literal
// stage name it matched no row: a purge scoped the way the listing is filtered
// answered {"deleted":0} having removed nothing, and the stats for the same
// filter read as a gateway that had served no traffic.
func stageFilter(r *http.Request) string {
	if stage := r.URL.Query().Get("stage"); stage != stageAll {
		return stage
	}
	return ""
}

// encodeDimension renders one dimension's groups, keeping the highest-count
// `limit` of them when limit is positive.
//
// Each group carries its error and token totals, not just a count. The
// aggregation query has always produced all three; only the count used to
// survive the trip, which left the dashboard able to say which model was called
// most often but not which one consumed the tokens — usually a different model,
// and always the more useful question.
//
// An absent dimension encodes as {} rather than null, which clients index into
// without checking.
func encodeDimension(input map[string]requestlog.DimensionStat, limit int) map[string]any {
	encoded := make(map[string]any, len(input))
	for name, stat := range keepTop(input, limit) {
		encoded[name] = map[string]any{
			"count":    stat.Count,
			"errors":   stat.Errors,
			"tokens":   stat.Tokens,
			"cost_usd": stat.CostUSD,
			"unpriced": stat.Unpriced,
		}
	}
	return encoded
}

// encodePercentiles renders a measured distribution, or null when nothing was
// measured.
//
// Null rather than a zero-filled object: a gateway that has recorded no
// duration has no median, and a p50 of 0ms would be read as an implausibly fast
// one rather than as an absent measurement. Rows written before the columns
// existed, and non-streaming requests in the case of time to first token,
// legitimately carry nothing.
func encodePercentiles(p *requestlog.Percentiles) any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"p50":   p.P50,
		"p95":   p.P95,
		"p99":   p.P99,
		"max":   p.Max,
		"mean":  p.Mean,
		"count": p.Count,
	}
}

func keepTop(input map[string]requestlog.DimensionStat, limit int) map[string]requestlog.DimensionStat {
	if limit <= 0 || len(input) <= limit {
		return input
	}

	type item struct {
		name string
		stat requestlog.DimensionStat
	}
	items := make([]item, 0, len(input))
	for name, stat := range input {
		items = append(items, item{name: name, stat: stat})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].stat.Count != items[j].stat.Count {
			return items[i].stat.Count > items[j].stat.Count
		}
		return items[i].name < items[j].name
	})

	trimmed := make(map[string]requestlog.DimensionStat, limit)
	for i := 0; i < limit; i++ {
		trimmed[items[i].name] = items[i].stat
	}

	return trimmed
}

func encodeErrorGroups(groups []requestlog.ErrorGroup) []map[string]any {
	encoded := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		encoded = append(encoded, map[string]any{"message": group.Message, "count": group.Count})
	}
	return encoded
}

// encodeSeries renders the time series with the window it actually covers.
//
// `truncated` and the start/end are part of the payload rather than inferred by
// the client, because a series that stopped short of the requested range looks
// exactly like a gateway that went quiet.
func encodeSeries(stats requestlog.StatsResult) map[string]any {
	points := make([]map[string]any, 0, len(stats.Series))
	for _, point := range stats.Series {
		points = append(points, map[string]any{
			"start":             point.Start.UTC().Format(time.RFC3339),
			"requests":          point.Requests,
			"errors":            point.Errors,
			"prompt_tokens":     point.PromptTokens,
			"completion_tokens": point.CompletionTokens,
		})
	}
	encoded := map[string]any{"points": points, "truncated": stats.SeriesTruncated}
	if !stats.SeriesStart.IsZero() {
		encoded["start"] = stats.SeriesStart.UTC().Format(time.RFC3339)
		encoded["end"] = stats.SeriesEnd.UTC().Format(time.RFC3339)
	}
	return encoded
}
