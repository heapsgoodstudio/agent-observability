package server

import (
	"context"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

func TestLiveSessionProfileIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, indexedProfile, want string
		labels                     []string
		indexed                    bool
	}{
		{name: "before indexing", labels: []string{"stable-id", "stable-id"}, want: "stable-id"},
		{name: "older unlabelled events", labels: []string{"", "stable-id"}, want: "stable-id"},
		{name: "no labels", labels: []string{"", ""}},
		{name: "conflicting labels", labels: []string{"one", "two", "one"}},
		{name: "indexed identity wins", indexed: true, indexedProfile: "indexed-id", labels: []string{"old-label"}, want: "indexed-id"},
		{name: "legacy unattributed index", indexed: true, labels: []string{"stable-id"}, want: "stable-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			const id = "55a28b14-70f9-49d3-ad3f-df73e9ebd9e8"
			if tc.indexed {
				if err := db.UpsertSession(d, db.SessionRow{ID: id, Profile: tc.indexedProfile, Start: testNow - 300}); err != nil {
					t.Fatal(err)
				}
			}
			var events []map[string]string
			for _, label := range tc.labels {
				events = append(events, map[string]string{"_time": rfc3339(testNow - 10), "profile": label})
			}
			logs := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
				sessionProbeQuery(id): ndjson(map[string]string{"n": strconv.Itoa(len(events))}),
				// Use the literal projection so dropping the profile field fails
				// this test, even if the response builder still reads it.
				`session.id:"` + id + `" event.name:api_request | fields _time, cost_usd, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, model, query_source, skill.name, profile`: ndjson(events...),
				sessionToolsQuery(id): "", sessionPromptsQuery(id): "",
			}})
			defer logs.Close()
			metrics := emptyOverageVM(t)
			defer metrics.Close()
			out, err := buildProviderSession(context.Background(), sessionsOptions(metrics, logs, d), id, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if !out.Live || out.Row.Profile != tc.want || out.Row.Provider != "claude" {
				t.Fatalf("live session identity = %+v, want profile %q", out.Row, tc.want)
			}
			if !out.Row.CostKnown || !out.Row.UsageKnown || !out.Row.ToolsKnown {
				t.Fatalf("live Claude measurement coverage lost: %+v", out.Row)
			}
		})
	}
}

func TestSessionScopesSurviveProfileRename(t *testing.T) {
	for _, provider := range []profiles.Provider{profiles.Claude, profiles.Codex} {
		t.Run(string(provider), func(t *testing.T) {
			o := settingsFixture(t)
			o.Memo = store.New(store.TTL)
			p := regressionProfile(t, o, provider, "Original display name")
			other := regressionProfile(t, o, provider, "Renamed profile")
			const id = "55a28b14-70f9-49d3-ad3f-df73e9ebd9e8"
			for _, row := range []db.SessionRow{
				{ID: id, Provider: string(provider), Profile: p.ID, Start: testNow - 300, End: testNow - 10},
				{ID: "other-session", Provider: string(provider), Profile: other.ID},
			} {
				if err := db.UpsertSession(o.DB, row); err != nil {
					t.Fatal(err)
				}
			}
			metrics := httptest.NewServer(fakeVMHandler{instant: map[string]string{
				accountInfoQuery: successBody(t, vmResult{Metric: map[string]string{"profile": p.ID, "account": "account-id"}, Value: []any{testNow, "1"}}),
			}})
			defer metrics.Close()
			o.VM = &vm.Client{BaseURL: metrics.URL, HTTP: metrics.Client()}
			logs := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
				sessionProbeQuery(id): ndjson(map[string]string{"n": "1"}),
				// Old instrumentation may omit profile. The indexed ID must survive.
				sessionEventsQuery(id): ndjson(map[string]string{"_time": rfc3339(testNow - 10)}),
				sessionToolsQuery(id):  "", sessionPromptsQuery(id): "",
				sessionsListQueryFor(p.ID): "",
			}})
			defer logs.Close()
			o.VL = &vl.Client{BaseURL: logs.URL, HTTP: logs.Client()}
			if provider == profiles.Codex {
				if err := monitor.WriteJSON(o.DB, "codex.quota."+p.ID, monitor.QuotaSnapshot{Directory: p.Directory, Usage: codex.Usage{AccountID: "account-id", FetchedAt: time.Unix(testNow, 0)}}); err != nil {
					t.Fatal(err)
				}
			}
			accountKey := db.AccountKey(string(provider), "account-id", "")
			ctx := context.Background()
			for _, name := range []string{p.Name, other.Name} {
				p.Name = name
				saved, err := o.Profiles.Save(p)
				if err != nil || saved.ID != p.ID {
					t.Fatalf("rename changed identity: %+v %v", saved, err)
				}
				for _, req := range []*pb.ListSessionsRequest{
					{Provider: string(provider), Profile: p.ID},
					{Provider: string(provider), AccountKey: accountKey},
					{Provider: string(provider), AccountKey: accountKey, Profile: p.ID},
				} {
					list, err := buildScopedSessions(ctx, o, req, testNow)
					if err != nil {
						t.Fatal(err)
					}
					if len(list.Sessions) != 1 || list.Sessions[0].Id != id || list.Sessions[0].Profile != p.ID {
						t.Fatalf("scope changed after rename to %q: %+v", name, list)
					}
					detail, err := buildProviderSession(ctx, o, id, testNow)
					if err != nil {
						t.Fatal(err)
					}
					if detail.Row.Profile != p.ID || detail.Row.Provider != string(provider) {
						t.Fatalf("detail no longer matches list/account scope: %+v", detail.Row)
					}
				}
				if _, err := resolveDashboardScope(ctx, o, string(provider), accountKey, name, testNow); err == nil {
					t.Fatal("display name accepted as a profile ID")
				}
				if _, err := resolveDashboardScope(ctx, o, string(provider), accountKey, other.ID, testNow); err == nil {
					t.Fatal("scope widened to another profile with the same display name")
				}
			}
		})
	}
}
