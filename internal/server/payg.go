package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

// paygDays is how many local days the daily series covers, today included.
const paygDays = 30

// GetPayAsYouGo reads only the snapshots the monitor writes for one
// pay-as-you-go harness. Their dollars are the harness's estimates and are
// shown because no plan includes this usage; the allowance rule that hides
// Claude and Codex dollars does not apply here.
func (s *obsService) GetPayAsYouGo(ctx context.Context, req *connect.Request[pb.GetPayAsYouGoRequest]) (*connect.Response[pb.GetPayAsYouGoResponse], error) {
	o := s.o
	if o.DB == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("session database unavailable"))
	}
	out := &pb.GetPayAsYouGoResponse{Identity: identity()}
	if o.Profiles == nil {
		return connect.NewResponse(out), nil
	}
	ps, err := o.Profiles.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	harness := profiles.Provider(req.Msg.Harness)
	if harness == "" {
		harness = profiles.Omp
	}
	if !profiles.Detected(harness) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown pay-as-you-go harness %q", req.Msg.Harness))
	}
	var buckets []payg.Bucket
	oldest := int64(0)
	for _, p := range ps {
		if p.Provider != harness || !p.Enabled || (req.Msg.Profile != "" && p.ID != req.Msg.Profile) {
			continue
		}
		out.ConfiguredProfiles++
		var snap monitor.SpendSnapshot
		if monitor.ReadJSON(o.DB, monitor.SpendSnapshotKey(harness, p.ID), &snap) != nil || snap.CollectedAt == 0 {
			out.Errors = append(out.Errors, &pb.SectionError{Section: p.Name, Error: "Waiting for the first collection"})
			continue
		}
		if snap.Error != "" {
			out.Errors = append(out.Errors, &pb.SectionError{Section: p.Name, Error: snap.Error})
		}
		if oldest == 0 || snap.CollectedAt < oldest {
			oldest = snap.CollectedAt
		}
		buckets = append(buckets, snap.Usage...)
	}
	out.LastCollectedAt = float64(oldest)
	out.Month, out.Providers = buildPayg(buckets, time.Now())
	return connect.NewResponse(out), nil
}

// buildPayg folds usage buckets from any number of profiles into one row per
// inference provider, local and free ones included. Two profiles reading the same omp home would count the
// same requests twice, but the registry refuses a second profile on one
// directory, so buckets from different profiles are always distinct requests.
func buildPayg(buckets []payg.Bucket, now time.Time) (string, []*pb.PaygProvider) {
	now = now.In(time.Local)
	month := now.Format("2006-01")
	first := now.AddDate(0, 0, -(paygDays - 1)).Format("2006-01-02")
	type modelKey struct{ provider, model string }
	providers := map[string]*pb.PaygProvider{}
	days := map[string]map[string]*pb.PaygDay{}
	models := map[modelKey]*pb.PaygModel{}
	// Every provider with a request is listed, a local model's included. One
	// with no recorded cost (a local model, or one the harness cannot price)
	// has CostRecorded false, and its token usage is what a client shows. An
	// unpriced request counts as cost: it means the provider does charge.
	listed := map[string]bool{}
	for _, b := range buckets {
		p := providers[b.Provider]
		if p == nil {
			p = &pb.PaygProvider{Provider: b.Provider, MonthToDateComplete: true, Last_30DaysComplete: true}
			providers[b.Provider] = p
			days[b.Provider] = map[string]*pb.PaygDay{}
		}
		if b.Requests > 0 {
			listed[b.Provider] = true
		}
		if b.CostUSD > 0 || (!b.CostKnown && b.Requests > 0) {
			p.CostRecorded = true
		}
		if b.Requests > 0 && b.Day > p.LastActiveDay {
			p.LastActiveDay = b.Day
		}
		tokens := float64(b.Input + b.Output + b.CacheRead + b.CacheWrite)
		if b.Day >= first {
			p.Last_30DaysUsd += b.CostUSD
			p.Last_30DaysTokens += tokens
			p.Last_30DaysComplete = p.Last_30DaysComplete && b.CostKnown
			d := days[b.Provider][b.Day]
			if d == nil {
				d = &pb.PaygDay{Day: b.Day}
				days[b.Provider][b.Day] = d
			}
			d.CostUsd += b.CostUSD
			d.Requests += float64(b.Requests)
			d.Tokens += tokens
		}
		if len(b.Day) >= 7 && b.Day[:7] == month {
			p.MonthToDateUsd += b.CostUSD
			p.MonthToDateTokens += tokens
			p.MonthToDateComplete = p.MonthToDateComplete && b.CostKnown
			k := modelKey{b.Provider, b.Model}
			m := models[k]
			if m == nil {
				m = &pb.PaygModel{Model: b.Model, CostComplete: true}
				models[k] = m
			}
			m.CostUsd += b.CostUSD
			m.CostComplete = m.CostComplete && b.CostKnown
			m.Requests += float64(b.Requests)
			m.Input += float64(b.Input)
			m.Output += float64(b.Output)
			m.CacheRead += float64(b.CacheRead)
			m.CacheWrite += float64(b.CacheWrite)
		}
	}
	var out []*pb.PaygProvider
	for name, p := range providers {
		if !listed[name] {
			continue
		}
		for i := 0; i < paygDays; i++ {
			day := now.AddDate(0, 0, i-(paygDays-1)).Format("2006-01-02")
			d := days[name][day]
			if d == nil {
				d = &pb.PaygDay{Day: day}
			}
			p.Days = append(p.Days, d)
		}
		for k, m := range models {
			if k.provider == name {
				p.Models = append(p.Models, m)
			}
		}
		sort.Slice(p.Models, func(i, j int) bool {
			a, b := p.Models[i], p.Models[j]
			if a.CostUsd != b.CostUsd {
				return a.CostUsd > b.CostUsd
			}
			if ta, tb := a.Input+a.Output+a.CacheRead+a.CacheWrite, b.Input+b.Output+b.CacheRead+b.CacheWrite; ta != tb {
				return ta > tb
			}
			return a.Model < b.Model
		})
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MonthToDateUsd != out[j].MonthToDateUsd {
			return out[i].MonthToDateUsd > out[j].MonthToDateUsd
		}
		if out[i].MonthToDateTokens != out[j].MonthToDateTokens {
			return out[i].MonthToDateTokens > out[j].MonthToDateTokens
		}
		return out[i].Provider < out[j].Provider
	})
	return month, out
}
