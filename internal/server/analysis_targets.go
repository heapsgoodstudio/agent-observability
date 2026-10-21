package server

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
)

func (s *obsService) GetAnalysisModels(ctx context.Context, req *connect.Request[pb.GetAnalysisModelsRequest]) (*connect.Response[pb.GetAnalysisModelsResponse], error) {
	if s.o.Profiles == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("Configure an evaluator profile in Settings"))
	}
	ps, err := s.o.Profiles.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("Evaluator profiles could not be loaded"))
	}
	for _, p := range ps {
		if p.ID != req.Msg.Profile || string(p.Provider) != req.Msg.Provider {
			continue
		}
		if !p.Enabled {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("Evaluator profile is disabled"))
		}
		if st, err := os.Stat(p.Directory); err != nil || !st.IsDir() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("Evaluator profile directory is unavailable"))
		}
		binary := analysis.Binary(string(p.Provider), s.o.AnalysisBinaries[string(p.Provider)])
		if binary == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("Evaluator CLI is not installed"))
		}
		models, err := s.modelCatalog.List(ctx, analysis.ModelTarget{Provider: string(p.Provider), Directory: p.Directory, Binary: binary})
		if err != nil {
			// CLI diagnostics can contain credential or configuration contents.
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("Could not load models from the selected profile. Check its CLI configuration and try again"))
		}
		response := &pb.GetAnalysisModelsResponse{}
		for _, model := range models {
			response.Models = append(response.Models, &pb.AnalysisModel{Id: model.ID, Name: model.Name, Provider: model.Provider})
		}
		return connect.NewResponse(response), nil
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("Evaluator profile was not found for this provider"))
}

func (s *obsService) analysisTargets(ctx context.Context) []*pb.AnalysisTarget {
	if s.o.Profiles == nil {
		return nil
	}
	ps, err := s.o.Profiles.List()
	if err != nil {
		return nil
	}
	observed := map[string]string{}
	if rows, err := buildConfiguredProfiles(ctx, s.o, s.o.now()); err == nil {
		for _, p := range rows.Profiles {
			observed[p.Profile] = p.Account
		}
	}
	out := []*pb.AnalysisTarget{}
	for _, p := range ps {
		target := &pb.AnalysisTarget{Provider: string(p.Provider), Profile: p.ID, Name: p.Name, Account: observed[p.ID], Available: p.Enabled}
		if !p.Enabled {
			target.UnavailableReason = "Profile is disabled"
		}
		if st, err := os.Stat(p.Directory); err != nil || !st.IsDir() {
			target.Available = false
			target.UnavailableReason = "Profile directory unavailable"
		}
		if analysis.Binary(string(p.Provider), s.o.AnalysisBinaries[string(p.Provider)]) == "" {
			target.Available = false
			target.UnavailableReason = "CLI not installed"
		}
		if p.Provider == "claude" {
			target.Models = []string{"sonnet", "opus", "haiku"}
		}
		out = append(out, target)
	}
	return out
}
