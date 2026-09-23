package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"
)

const (
	dayLayout = "2006-01-02"
	// maxUsageDays bounds one report (a leap year).
	maxUsageDays = 366
	// defaultUsageDays is the default report length, ending today.
	defaultUsageDays = 7
)

// getUsage reports token and cost totals per UTC day and domain, behind
// X-Internal-Token. from and to are inclusive days.
func (s *Server) getUsage(ctx context.Context, in *UsageInput) (*GetUsageOutput, error) {
	if !s.validInternalToken(in.Token) {
		return nil, NewAPIError(http.StatusUnauthorized, CodeUnauthorized, "missing or invalid X-Internal-Token")
	}
	now := s.d.Now().UTC()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if v := strings.TrimSpace(in.To); v != "" {
		d, err := time.Parse(dayLayout, v)
		if err != nil {
			return nil, NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "to must be a date (YYYY-MM-DD)")
		}
		to = d
	}
	from := to.AddDate(0, 0, -(defaultUsageDays - 1))
	if v := strings.TrimSpace(in.From); v != "" {
		d, err := time.Parse(dayLayout, v)
		if err != nil {
			return nil, NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "from must be a date (YYYY-MM-DD)")
		}
		from = d
	}
	if from.After(to) {
		return nil, NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "from must not be after to")
	}
	if days := int(to.Sub(from).Hours()/24) + 1; days > maxUsageDays {
		return nil, NewAPIError(http.StatusBadRequest, CodeInvalidRequest, "the range must not exceed 366 days")
	}

	rows, err := s.d.Store.UsageReport(ctx, from, to.AddDate(0, 0, 1), strings.TrimSpace(in.DomainKey))
	if err != nil {
		return nil, s.apiError(ctx, "usage report", err)
	}
	out := UsageReport{From: from, To: to, Rows: make([]UsageRow, 0, len(rows))}
	var total float64
	for _, r := range rows {
		out.Rows = append(out.Rows, UsageRow{
			Day:            r.Day.UTC().Format(dayLayout),
			DomainKey:      r.DomainKey,
			Jobs:           r.Jobs,
			Calls:          r.Calls,
			InputTokens:    r.InputTokens,
			OutputTokens:   r.OutputTokens,
			ThinkingTokens: r.ThinkingTokens,
			VideoSeconds:   r.VideoSeconds,
			CostUSD:        r.CostUSD,
		})
		total += r.CostUSD
	}
	out.TotalCostUSD = roundUSD(total)
	return &GetUsageOutput{Body: out}, nil
}
