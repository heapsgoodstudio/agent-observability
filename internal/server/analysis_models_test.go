package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func TestAnalysisModelsResolveAndIsolateEnabledEvaluatorProfiles(t *testing.T) {
	o := settingsFixture(t)
	a := regressionProfile(t, o, profiles.Codex, "One")
	b := regressionProfile(t, o, profiles.Codex, "Two")
	o.AnalysisBinaries = map[string]string{"codex": "/usr/bin/true"}
	var selected []analysis.ModelTarget
	s := &obsService{o: o, modelCatalog: analysis.ModelCatalog{Discover: func(_ context.Context, target analysis.ModelTarget) ([]analysis.ModelOption, error) {
		selected = append(selected, target)
		return []analysis.ModelOption{{ID: "model", Name: "Model"}}, nil
	}}}
	for _, profile := range []profiles.Profile{a, b, a} {
		r, err := s.GetAnalysisModels(context.Background(), connect.NewRequest(&pb.GetAnalysisModelsRequest{Provider: "codex", Profile: profile.ID}))
		if err != nil || len(r.Msg.Models) != 1 || r.Msg.Models[0].Id != "model" {
			t.Fatalf("%+v %v", r, err)
		}
	}
	if len(selected) != 2 || selected[0].Directory != a.Directory || selected[1].Directory != b.Directory {
		t.Fatalf("credential homes mixed: %+v", selected)
	}
	for _, request := range []*pb.GetAnalysisModelsRequest{{Provider: "claude", Profile: a.ID}, {Provider: "codex", Profile: a.Directory}, {Provider: "codex"}} {
		if _, err := s.GetAnalysisModels(context.Background(), connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("invalid selection accepted: %v", err)
		}
	}
	a.Enabled = false
	if _, err := o.Profiles.Save(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAnalysisModels(context.Background(), connect.NewRequest(&pb.GetAnalysisModelsRequest{Provider: "codex", Profile: a.ID})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("cached disabled profile served: %v", err)
	}
	if len(selected) != 2 {
		t.Fatal("invalid requests spawned a catalog lookup")
	}
}

func TestAnalysisModelsNeverExposeRawCLIErrors(t *testing.T) {
	o := settingsFixture(t)
	p := regressionProfile(t, o, profiles.Codex, "Work")
	o.AnalysisBinaries = map[string]string{"codex": "/usr/bin/true"}
	s := &obsService{o: o, modelCatalog: analysis.ModelCatalog{Discover: func(context.Context, analysis.ModelTarget) ([]analysis.ModelOption, error) {
		return nil, errors.New("SECRET_CREDENTIAL_VALUE")
	}}}
	_, err := s.GetAnalysisModels(context.Background(), connect.NewRequest(&pb.GetAnalysisModelsRequest{Provider: "codex", Profile: p.ID}))
	if connect.CodeOf(err) != connect.CodeUnavailable || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe discovery error: %v", err)
	}
}
