package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func (s *obsService) profileStore() (*profiles.Store, error) {
	if s.o.Profiles == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("profile settings are not configured"))
	}
	return s.o.Profiles, nil
}

func configuredProfile(p profiles.Profile) *pb.ConfiguredProfile {
	status := p.WiringStatus
	if !p.Enabled {
		status = "disabled"
	}
	return &pb.ConfiguredProfile{Id: p.ID, Provider: string(p.Provider), Name: p.Name, Directory: p.Directory, Enabled: p.Enabled, IsPrimary: p.Primary, Status: status, Detail: p.WiringError}
}

func importProto(st monitor.ImportStatus) *pb.ImportStatus {
	return &pb.ImportStatus{State: st.State, Profile: st.Profile, Message: st.Message, StartedAt: float64(st.StartedAt), FinishedAt: float64(st.FinishedAt), Sessions: float64(st.Sessions)}
}

func (s *obsService) GetSettings(ctx context.Context, req *connect.Request[pb.GetSettingsRequest]) (*connect.Response[pb.GetSettingsResponse], error) {
	st, err := s.profileStore()
	if err != nil {
		return nil, err
	}
	ps, err := st.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	home, _ := os.UserHomeDir()
	out := &pb.GetSettingsResponse{ClaudeDefaultDir: filepath.Join(home, ".claude"), CodexDefaultDir: filepath.Join(home, ".codex"), OmpDefaultDir: filepath.Join(home, ".omp", "agent"), OpencodeDefaultDir: filepath.Join(home, ".local", "share", "opencode"), HistoryImport: &pb.ImportStatus{State: "idle"}}
	for _, p := range ps {
		row := configuredProfile(p)
		if s.o.DB != nil {
			var seen int64
			s.o.DB.QueryRowContext(ctx, `SELECT coalesce(max("end"),0) FROM sessions WHERE profile = ?`, p.ID).Scan(&seen)
			row.LastSeen = float64(seen)
		}
		if fi, e := os.Stat(p.Directory); e != nil || !fi.IsDir() {
			row.Status = "missing"
			row.Detail = "Profile directory is unavailable"
		}
		out.Profiles = append(out.Profiles, row)
	}
	if s.o.Monitor != nil {
		out.HistoryImport = importProto(s.o.Monitor.Status())
	}
	return connect.NewResponse(out), nil
}

func (s *obsService) SaveProfile(ctx context.Context, req *connect.Request[pb.SaveProfileRequest]) (*connect.Response[pb.SaveProfileResponse], error) {
	st, err := s.profileStore()
	if err != nil {
		return nil, err
	}
	p := req.Msg.Profile
	if p == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("profile is required"))
	}
	var previous profiles.Profile
	if p.Id != "" {
		registry, err := st.Load()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, old := range registry.Profiles {
			if old.ID == p.Id {
				previous = old
				break
			}
		}
	}
	// Primary is not something a person manages: it is only the internal
	// fallback Claude profile (advice with no execution profile, the legacy
	// single-account fields, config.env's PROFILE_DIR), and normalizePrimary
	// assigns it to the first enabled profile when none is set. So a client's
	// is_primary is ignored and an edit keeps whatever the profile had.
	saved, err := st.Save(profiles.Profile{ID: p.Id, Provider: profiles.Provider(p.Provider), Name: p.Name, Directory: p.Directory, Enabled: p.Enabled, Primary: previous.Primary})
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if saved.Provider == profiles.Codex && s.o.DB != nil && (p.Id == "" || !saved.Enabled || !previous.Enabled || previous.Directory != saved.Directory) {
		// A different home may belong to a different account. Do not show its
		// predecessor's apparently fresh allowance while the next poll starts.
		if err := monitor.WriteJSON(s.o.DB, "codex.quota."+saved.ID, monitor.QuotaSnapshot{Directory: saved.Directory, UsageError: "Waiting for a plan snapshot for this profile"}); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	if s.o.Monitor != nil && saved.Enabled {
		if p.Id == "" || !previous.Enabled || previous.Directory != saved.Directory {
			if err := s.o.Monitor.Enqueue(saved.ID); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		} else {
			s.o.Monitor.Start(saved.ID, false)
		}
	}
	return connect.NewResponse(&pb.SaveProfileResponse{Profile: configuredProfile(saved)}), nil
}

func (s *obsService) RemoveProfile(ctx context.Context, req *connect.Request[pb.RemoveProfileRequest]) (*connect.Response[pb.RemoveProfileResponse], error) {
	st, err := s.profileStore()
	if err != nil {
		return nil, err
	}
	ps, err := st.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	wasCodex := false
	for _, p := range ps {
		if p.ID == req.Msg.Id {
			wasCodex = p.Provider == profiles.Codex
		}
	}
	if err = st.Remove(req.Msg.Id); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if wasCodex && s.o.DB != nil {
		if err := monitor.WriteJSON(s.o.DB, "codex.quota."+req.Msg.Id, monitor.QuotaSnapshot{UsageError: "Profile disconnected"}); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&pb.RemoveProfileResponse{}), nil
}

func (s *obsService) RepairProfile(ctx context.Context, req *connect.Request[pb.RepairProfileRequest]) (*connect.Response[pb.RepairProfileResponse], error) {
	st, err := s.profileStore()
	if err != nil {
		return nil, err
	}
	p, err := st.Repair(req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&pb.RepairProfileResponse{Profile: configuredProfile(p)}), nil
}

func (s *obsService) ImportHistory(ctx context.Context, req *connect.Request[pb.ImportHistoryRequest]) (*connect.Response[pb.ImportHistoryResponse], error) {
	if s.o.Monitor == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("history importer unavailable"))
	}
	st, err := s.o.Monitor.Start(req.Msg.Profile, true)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&pb.ImportHistoryResponse{HistoryImport: importProto(st)}), nil
}

func (s *obsService) SetAccountHidden(ctx context.Context, req *connect.Request[pb.SetAccountHiddenRequest]) (*connect.Response[pb.SetAccountHiddenResponse], error) {
	st, err := s.profileStore()
	if err != nil {
		return nil, err
	}
	if err := st.SetAccountHidden(req.Msg.AccountKey, req.Msg.Hidden); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&pb.SetAccountHiddenResponse{}), nil
}

// Settings mutate local files, so reject cross-origin browser requests and
// non-loopback Host headers (including DNS-rebinding targets).
func protectSettingsWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutation := false
		for _, method := range []string{"SaveProfile", "RemoveProfile", "RepairProfile", "ImportHistory", "StartAnalysis", "StartAdvice", "GetAnalysisModels", "SetAccountHidden"} {
			if strings.HasSuffix(r.URL.Path, "/"+method) {
				mutation = true
			}
		}
		if mutation {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			ip := net.ParseIP(strings.Trim(host, "[]"))
			if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				http.Error(w, "settings are local only", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "cross-origin settings request rejected", http.StatusForbidden)
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-site settings request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *obsService) options() Options {
	o := s.o
	if o.Profiles == nil {
		return o
	}
	ps, err := o.Profiles.List()
	if err != nil {
		return o
	}
	o.ProfileDirs = nil
	o.PrimaryProfile = ""
	for _, p := range ps {
		if p.Enabled && p.Provider == profiles.Claude {
			if p.Primary {
				o.PrimaryProfile = p.ID
				o.ProfileDirs = append([]string{p.Directory}, o.ProfileDirs...)
			} else {
				o.ProfileDirs = append(o.ProfileDirs, p.Directory)
			}
		}
	}
	if o.PrimaryProfile == "" {
		for _, p := range ps {
			if p.Enabled && p.Provider == profiles.Claude {
				o.PrimaryProfile = p.ID
				break
			}
		}
	}
	return o
}

func providerOf(r db.SessionRow) string {
	if r.Provider == "" {
		return "claude"
	}
	return r.Provider
}
func checkProvider(p string) error {
	if p != "" && p != "claude" && p != "codex" && !profiles.Detected(profiles.Provider(p)) {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown provider %q", p))
	}
	return nil
}
