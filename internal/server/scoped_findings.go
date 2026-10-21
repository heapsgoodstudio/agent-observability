package server

import (
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/rules"
)

func indexedFindings(rows []db.SessionRow) *pb.GetFindingsResponse {
	facts := make([]rules.IndexedSession, 0, len(rows))
	for _, r := range rows {
		facts = append(facts, rules.IndexedSession{ID: r.ID, Requests: r.Requests, Errors: r.ErrorCount, UsageKnown: providerOf(r) == "claude" || r.UsageKnown, ErrorsKnown: r.ErrorsKnown, HasTranscript: r.HasTranscript})
	}
	findings := rules.RunIndexed(facts)
	return &pb.GetFindingsResponse{Identity: identity(), IndexOk: true, VmOk: true, VlOk: true, Findings: findingsToProto(findings), BadgeCount: badgeCount(findings)}
}
