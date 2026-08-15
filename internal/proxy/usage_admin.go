package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/store"
)

type valuedUsage struct {
	Account  string              `json:"account,omitempty"`
	Model    string              `json:"model,omitempty"`
	Usage    store.UsageCounters `json:"usage"`
	CostUSD  float64             `json:"cost_usd"`
	Unpriced bool                `json:"unpriced,omitempty"`
}

type fiveHourEstimate struct {
	Account          string  `json:"account"`
	From             int64   `json:"from"`
	To               int64   `json:"to"`
	ResetsAt         string  `json:"resets_at,omitempty"`
	UsedPercentDelta float64 `json:"used_percent_delta"`
	ObservedCostUSD  float64 `json:"observed_cost_usd"`
	FullWindowUSD    float64 `json:"full_window_usd"`
}

type usageDashboardResponse struct {
	From              int64              `json:"from"`
	To                int64              `json:"to"`
	Totals            valuedUsage        `json:"totals"`
	ByModel           []valuedUsage      `json:"by_model"`
	ByAccount         []valuedUsage      `json:"by_account"`
	UnpricedModels    []string           `json:"unpriced_models"`
	FiveHourEstimates []fiveHourEstimate `json:"five_hour_estimates"`
}

func (s *Server) usageDashboard(w http.ResponseWriter, r *http.Request) {
	if err := s.accounting.Flush(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to persist pending usage")
		return
	}
	now := time.Now()
	from := now.Add(-7 * 24 * time.Hour).Unix()
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "from must be an epoch timestamp in milliseconds")
			return
		}
		from = parsed / 1000
	}
	buckets, err := s.store.UsageBuckets(r.Context(), from)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query usage")
		return
	}
	prices, err := s.store.ModelPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query model prices")
		return
	}
	response := buildUsageDashboard(buckets, prices, from, now.Unix())
	snapshots, err := s.store.SubscriptionUsageSnapshots(r.Context())
	if err == nil {
		response.FiveHourEstimates = buildFiveHourEstimates(snapshots, prices)
	}
	writeJSON(w, http.StatusOK, response)
}

func buildUsageDashboard(buckets []store.UsageBucket, prices []store.ModelPrice, from, to int64) usageDashboardResponse {
	byModel := make(map[string]*valuedUsage)
	byAccount := make(map[string]*valuedUsage)
	unpriced := make(map[string]bool)
	response := usageDashboardResponse{From: from * 1000, To: to * 1000, ByModel: []valuedUsage{}, ByAccount: []valuedUsage{}, UnpricedModels: []string{}, FiveHourEstimates: []fiveHourEstimate{}}
	for _, bucket := range buckets {
		price, priced := matchingPrice(prices, bucket.Model, bucket.BucketStart)
		cost := 0.0
		if priced {
			cost = usageCost(bucket.Counters, price)
		} else {
			unpriced[bucket.Model] = true
			response.Totals.Unpriced = true
		}
		response.Totals.Usage.Add(bucket.Counters)
		response.Totals.CostUSD += cost
		model := byModel[bucket.Model]
		if model == nil {
			model = &valuedUsage{Model: bucket.Model}
			byModel[bucket.Model] = model
		}
		model.Usage.Add(bucket.Counters)
		model.CostUSD += cost
		model.Unpriced = model.Unpriced || !priced
		account := byAccount[bucket.Account]
		if account == nil {
			account = &valuedUsage{Account: bucket.Account}
			byAccount[bucket.Account] = account
		}
		account.Usage.Add(bucket.Counters)
		account.CostUSD += cost
		account.Unpriced = account.Unpriced || !priced
	}
	for _, value := range byModel {
		response.ByModel = append(response.ByModel, *value)
	}
	for _, value := range byAccount {
		response.ByAccount = append(response.ByAccount, *value)
	}
	for model := range unpriced {
		response.UnpricedModels = append(response.UnpricedModels, model)
	}
	return response
}

func matchingPrice(prices []store.ModelPrice, model string, at int64) (store.ModelPrice, bool) {
	var best store.ModelPrice
	bestSpecificity := -1
	for _, price := range prices {
		if price.EffectiveFrom > at {
			continue
		}
		pattern := price.ModelPattern
		prefix := strings.TrimSuffix(pattern, "*")
		matches := pattern == model || (strings.HasSuffix(pattern, "*") && strings.HasPrefix(model, prefix))
		if !matches {
			continue
		}
		specificity := len(prefix)
		if pattern == model {
			specificity += 1000
		}
		if specificity > bestSpecificity || (specificity == bestSpecificity && price.EffectiveFrom > best.EffectiveFrom) {
			best, bestSpecificity = price, specificity
		}
	}
	return best, bestSpecificity >= 0
}

func usageCost(usage store.UsageCounters, price store.ModelPrice) float64 {
	return (float64(usage.InputTokens)*price.InputUSDPerMTok +
		float64(usage.OutputTokens)*price.OutputUSDPerMTok +
		float64(usage.CacheCreation5mTokens)*price.CacheCreation5mUSDPerMTok +
		float64(usage.CacheCreation1hTokens)*price.CacheCreation1hUSDPerMTok +
		float64(usage.CacheReadTokens)*price.CacheReadUSDPerMTok) / 1_000_000
}

type windowKey struct {
	accountID int64
	resetsAt  string
}

type windowSpan struct {
	first, last store.SubscriptionUsageSnapshot
}

// Five seconds leaves room beyond the observed one-second upstream drift while
// staying negligible beside the five hours between genuine reset instants.
const fiveHourResetMergeToleranceSeconds int64 = 5

// minMeasurablePercentDelta discards a window whose utilization barely moved.
// A reading carries up to half a point of rounding error, so anything below half
// a point extrapolates to more error than answer.
//
// It also keeps a denominator too small to divide by out of the arithmetic.
// Readings the sampler stored before it began rounding differ by around 1e-15
// when they report the same whole percent, because multiplying the 0..1 fraction
// out leaves a residue. The manual refresh path reaches the same floor from the
// other direction: the OAuth surface reports its own 0..100 value in fractions
// of a point, so two of those readings can differ by a quarter of a point and
// mean nothing by it.
const minMeasurablePercentDelta = 0.5

func sameFiveHourReset(left, right string) bool {
	if left == right {
		return true
	}
	leftSeconds, err := strconv.ParseInt(left, 10, 64)
	if err != nil {
		return false
	}
	rightSeconds, err := strconv.ParseInt(right, 10, 64)
	if err != nil {
		return false
	}
	if leftSeconds < rightSeconds {
		leftSeconds, rightSeconds = rightSeconds, leftSeconds
	}
	return leftSeconds-rightSeconds <= fiveHourResetMergeToleranceSeconds
}

// buildFiveHourEstimates anchors every window to its own earliest reading rather
// than pairing neighbouring ones. Utilization is reported in whole percent, so
// the closer two readings sit the more certain their difference is zero, and
// denser sampling would defeat itself. Anchoring lets the denominator accumulate
// across the window, which is also what makes the figure more accurate as the
// window fills.
//
// The anchor need not be the true start of the window. Only the numerator and
// denominator have to describe the same span, so quota the account spent before
// the relay first saw it is excluded from both.
func buildFiveHourEstimates(snapshots []store.SubscriptionUsageSnapshot, prices []store.ModelPrice) []fiveHourEstimate {
	windows := make(map[windowKey]*windowSpan)
	var order []windowKey
	for index := len(snapshots) - 1; index >= 0; index-- {
		current := snapshots[index]
		if current.ResetsAt == "" {
			continue
		}
		key := windowKey{current.AccountID, current.ResetsAt}
		span := windows[key]
		if span == nil {
			for _, candidate := range order {
				if candidate.accountID == current.AccountID && sameFiveHourReset(candidate.resetsAt, current.ResetsAt) {
					key = candidate
					span = windows[key]
					windows[windowKey{current.AccountID, current.ResetsAt}] = span
					break
				}
			}
		}
		if span == nil {
			span = &windowSpan{first: current, last: current}
			windows[key] = span
			order = append(order, key)
			continue
		}
		if current.ObservedAt < span.first.ObservedAt {
			span.first = current
		}
		if current.ObservedAt > span.last.ObservedAt {
			span.last = current
		}
	}
	var estimates []fiveHourEstimate
	for _, key := range order {
		span := windows[key]
		// A window holding one reading compares it against itself and falls out
		// here along with a window whose utilization never moved.
		deltaPercent := span.last.UsedPercent - span.first.UsedPercent
		if deltaPercent < minMeasurablePercentDelta {
			continue
		}
		cost := snapshotDeltaCost(span.first.Totals, span.last.Totals, prices, span.last.ObservedAt/1000)
		if cost <= 0 {
			continue
		}
		estimates = append(estimates, fiveHourEstimate{Account: span.last.Account, From: span.first.ObservedAt,
			To: span.last.ObservedAt, ResetsAt: key.resetsAt, UsedPercentDelta: deltaPercent,
			ObservedCostUSD: cost, FullWindowUSD: cost / deltaPercent * 100})
	}
	return estimates
}

func snapshotDeltaCost(before, after map[string]store.UsageCounters, prices []store.ModelPrice, at int64) float64 {
	cost := 0.0
	for model, current := range after {
		prior := before[model]
		delta := store.UsageCounters{
			InputTokens: max64(0, current.InputTokens-prior.InputTokens), OutputTokens: max64(0, current.OutputTokens-prior.OutputTokens),
			CacheCreation5mTokens: max64(0, current.CacheCreation5mTokens-prior.CacheCreation5mTokens),
			CacheCreation1hTokens: max64(0, current.CacheCreation1hTokens-prior.CacheCreation1hTokens),
			CacheReadTokens:       max64(0, current.CacheReadTokens-prior.CacheReadTokens),
		}
		if price, ok := matchingPrice(prices, model, at); ok {
			cost += usageCost(delta, price)
		}
	}
	return cost
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Server) listModelPrices(w http.ResponseWriter, r *http.Request) {
	prices, err := s.store.ModelPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query model prices")
		return
	}
	views := make([]store.ModelPrice, len(prices))
	for index, price := range prices {
		views[index] = price
		views[index].EffectiveFrom *= 1000
		views[index].CreatedAt *= 1000
	}
	writeJSON(w, http.StatusOK, map[string]any{"prices": views})
}

func (s *Server) saveModelPrice(w http.ResponseWriter, r *http.Request) {
	var price store.ModelPrice
	if err := decodeAdminJSON(w, r, &price); err != nil {
		return
	}
	price.EffectiveFrom /= 1000
	saved, err := s.store.SaveModelPrice(r.Context(), price)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	saved.EffectiveFrom *= 1000
	saved.CreatedAt *= 1000
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) clearUsageAccounting(w http.ResponseWriter, r *http.Request) {
	if err := s.accounting.Clear(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to clear usage accounting")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}
