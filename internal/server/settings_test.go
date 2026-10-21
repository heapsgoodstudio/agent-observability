package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1/claudeobsv1connect"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

func settingsFixture(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "state"), 0700)
	d, err := db.Open(filepath.Join(dir, "state", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err = db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	return Options{InstallDir: dir, Profiles: profiles.New(dir), DB: d}
}

func TestSettingsManualProfileLifecycleWithoutTelemetry(t *testing.T) {
	o := settingsFixture(t)
	srv := httptest.NewServer(Handler(o))
	defer srv.Close()
	c := claudeobsv1connect.NewObsServiceClient(http.DefaultClient, srv.URL)
	ctx := context.Background()
	initial, err := c.GetSettings(ctx, connect.NewRequest(&pb.GetSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Msg.Profiles) != 0 {
		t.Fatal("Settings must not discover any custom or default directories")
	}
	dir := t.TempDir()
	saved, err := c.SaveProfile(ctx, connect.NewRequest(&pb.SaveProfileRequest{Profile: &pb.ConfiguredProfile{Provider: "codex", Name: "Work", Directory: dir, Enabled: true}}))
	if err != nil {
		t.Fatal(err)
	}
	id := saved.Msg.Profile.Id
	if id == "" {
		t.Fatal("missing stable profile ID")
	}
	roster, err := c.ListProfiles(ctx, connect.NewRequest(&pb.ListProfilesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Msg.Profiles) != 1 || roster.Msg.Profiles[0].Profile != id || roster.Msg.Profiles[0].Provider != "codex" {
		t.Fatalf("roster without metrics: %v", roster.Msg)
	}
	if err := db.UpsertSession(o.DB, db.SessionRow{ID: "codex:" + id + ":thread", Provider: "codex", Profile: id, NativeID: "thread", CostUSD: 2, CostKnown: true, UsageKnown: true}); err != nil {
		t.Fatal(err)
	}
	saved.Msg.Profile.Name = "Renamed"
	edited, err := c.SaveProfile(ctx, connect.NewRequest(&pb.SaveProfileRequest{Profile: saved.Msg.Profile}))
	if err != nil {
		t.Fatal(err)
	}
	if edited.Msg.Profile.Id != id {
		t.Fatal("display name changed identity")
	}
	summary, err := c.GetMonitoring(ctx, connect.NewRequest(&pb.GetMonitoringRequest{Profile: id}))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Msg.Sessions != 1 || summary.Msg.EstimatedCostUsd != 0 || summary.Msg.Billing == nil || summary.Msg.Billing.EstimatedOverageUsd != nil {
		t.Fatalf("summary: %v", summary.Msg)
	}
	if _, err = c.RemoveProfile(ctx, connect.NewRequest(&pb.RemoveProfileRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListSessions(o.DB, id)
	if err != nil || len(rows) != 1 {
		t.Fatal("disconnect deleted history", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("exporter")) {
		t.Fatal("owned wiring remains after disconnect")
	}
}

func TestSettingsWritesRejectCrossOriginAndRebinding(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := protectSettingsWrites(next)
	cases := []struct {
		host, origin, site string
		want               int
	}{
		{"localhost:8427", "http://localhost:8427", "same-origin", 204},
		{"127.0.0.1:8427", "", "", 204},
		{"[::1]:8427", "", "", 204},
		{"localhost:8427", "https://evil.example", "cross-site", 403},
		{"evil.example:8427", "", "", 403},
		{"localhost:8427", "null", "", 403},
	}
	for _, method := range []string{"SaveProfile", "SetAccountHidden"} {
		for _, tc := range cases {
			t.Run(method+" "+tc.host+tc.origin, func(t *testing.T) {
				req := httptest.NewRequest("POST", "http://"+tc.host+"/claudeobs.v1.ObsService/"+method, nil)
				req.Header.Set("Origin", tc.origin)
				req.Header.Set("Sec-Fetch-Site", tc.site)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if w.Code != tc.want {
					t.Fatalf("got %d want %d", w.Code, tc.want)
				}
			})
		}
	}
}

func TestCodexIngestTagsAndHonorsDisabledProfiles(t *testing.T) {
	o := settingsFixture(t)
	p, err := o.Profiles.Save(profiles.Profile{Provider: profiles.Codex, Directory: t.TempDir(), Name: "Codex", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	forwarded := 0
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		if r.URL.Path != "/insert/opentelemetry/v1/logs" {
			t.Errorf("path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		data := &logspb.LogsData{}
		if err := proto.Unmarshal(body, data); err != nil {
			t.Error(err)
			return
		}
		attrs := map[string]string{}
		for _, a := range data.ResourceLogs[0].Resource.Attributes {
			attrs[a.Key] = a.Value.GetStringValue()
		}
		if attrs["provider"] != "codex" || attrs["profile"] != p.ID {
			t.Errorf("attributes %v", attrs)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer store.Close()
	o.VL = &vl.Client{BaseURL: store.URL}
	payload, _ := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{}}}})
	post := func() int {
		r := httptest.NewRequest("POST", "http://localhost:8427/ingest/codex/"+p.ID+"/v1/logs", bytes.NewReader(payload))
		r.Header.Set("Content-Type", "application/x-protobuf")
		w := httptest.NewRecorder()
		codexIngest(o).ServeHTTP(w, r)
		return w.Code
	}
	if got := post(); got != 200 {
		t.Fatalf("ingest %d", got)
	}
	p.Enabled = false
	if _, err = o.Profiles.Save(p); err != nil {
		t.Fatal(err)
	}
	if got := post(); got != 404 {
		t.Fatalf("disabled ingest %d", got)
	}
	if forwarded != 1 {
		t.Fatalf("forwarded %d times", forwarded)
	}
}

func TestCodexSessionsAvoidClaudeLiveAndBillingQueries(t *testing.T) {
	o := settingsFixture(t)
	for _, row := range []db.SessionRow{{ID: "c", Profile: "default", CostUSD: 3}, {ID: "codex:p:native", Provider: "codex", Profile: "p", NativeID: "native", CostUSD: 7, HasTranscript: true}} {
		if err := db.UpsertSession(o.DB, row); err != nil {
			t.Fatal(err)
		}
	}
	out, err := buildProviderSessions(context.Background(), o, "", "codex", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].Provider != "codex" || out.Sessions[0].CostKnown {
		t.Fatalf("sessions %v", out)
	}
	detail, err := buildProviderSession(context.Background(), o, "codex:p:native", 1)
	if err != nil {
		t.Fatal(err)
	}
	if detail.OverageObserved || detail.RealCostUsd != 0 || detail.Row.NativeId != "native" {
		t.Fatal("Claude billing leaked into Codex", detail)
	}
	if err = db.MarkMissing(o.DB, map[string]bool{}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	row, ok, err := db.GetSession(o.DB, "codex:p:native")
	if err != nil || !ok || !row.HasTranscript {
		t.Fatal("Claude missing sweep changed Codex", err)
	}
}

// Primary is an internal fallback, not something a client sets: a save that
// asks for it changes nothing, and editing the fallback profile keeps it.
func TestSaveProfileIgnoresClientPrimary(t *testing.T) {
	o := settingsFixture(t)
	svc := &obsService{o: o}
	save := func(id, dir, name string, primary bool) *pb.ConfiguredProfile {
		t.Helper()
		resp, err := svc.SaveProfile(context.Background(), connect.NewRequest(&pb.SaveProfileRequest{Profile: &pb.ConfiguredProfile{Id: id, Provider: "claude", Name: name, Directory: dir, Enabled: true, IsPrimary: primary}}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.Profile
	}
	first := save("", t.TempDir(), "first", false)
	second := save("", t.TempDir(), "second", true)
	if !first.IsPrimary || second.IsPrimary {
		t.Fatalf("first=%v second=%v; the first enabled profile is the fallback and a client cannot move it", first.IsPrimary, second.IsPrimary)
	}
	renamed := save(first.Id, first.Directory, "renamed", false)
	if !renamed.IsPrimary {
		t.Fatal("editing the fallback profile must keep it")
	}
}
