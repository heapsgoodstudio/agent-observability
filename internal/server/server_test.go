package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1/claudeobsv1connect"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>Agent Observability</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Handler(Options{StaticDir: dir, InstallDir: "/tmp/install", Version: "test"})
}

func TestServesStaticDashboard(t *testing.T) {
	srv := httptest.NewServer(testHandler(t))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestGetMeta(t *testing.T) {
	srv := httptest.NewServer(testHandler(t))
	defer srv.Close()
	client := claudeobsv1connect.NewObsServiceClient(http.DefaultClient, srv.URL)
	resp, err := client.GetMeta(context.Background(), connect.NewRequest(&claudeobsv1.GetMetaRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if m.Version != "test" || m.InstallDir != "/tmp/install" {
		t.Fatalf("meta = %+v", m)
	}
	if m.DashPort != 8427 || m.VmPort != 8428 || m.VlPort != 9428 {
		t.Fatalf("ports = %d %d %d", m.DashPort, m.VmPort, m.VlPort)
	}
	if m.Identity == nil || m.Identity.Hostname == "" || m.Identity.User == "" || m.Identity.GeneratedAt == "" {
		t.Fatalf("identity = %+v", m.Identity)
	}
}

// GetUsageSnapshot is the routing read and answers a plain GET, so it can be
// opened as a URL. The other reads stay POST only: the 405 on GetSpend is the
// control that shows the 200 comes from the option on this one RPC.
func TestUsageSnapshotAnswersAPlainGet(t *testing.T) {
	store := httptest.NewServer(fakeVMHandler{})
	defer store.Close()
	srv := httptest.NewServer(Handler(Options{StaticDir: t.TempDir(), VM: &vm.Client{BaseURL: store.URL, HTTP: store.Client()}}))
	defer srv.Close()
	get := func(path string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}

	status, body := get("/claudeobs.v1.ObsService/GetUsageSnapshot?encoding=json&message={}")
	if status != http.StatusOK {
		t.Fatalf("GET GetUsageSnapshot = %d: %s", status, body)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("GET GetUsageSnapshot is not JSON: %v: %s", err, body)
	}
	if _, ok := snapshot["schemaVersion"]; !ok {
		t.Fatalf("GET GetUsageSnapshot has no schemaVersion: %s", body)
	}

	if status, body := get("/claudeobs.v1.ObsService/GetUsageSnapshot?encoding=json"); status != http.StatusBadRequest {
		t.Fatalf("GET without message = %d, want 400: %s", status, body)
	}
	if status, _ := get("/claudeobs.v1.ObsService/GetSpend?encoding=json&message={}"); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET GetSpend = %d, want 405", status)
	}
}

func TestMachineID(t *testing.T) {
	out := `    "IOPlatformUUID" = "ABC-123-DEF"` + "\n"
	got := MachineID(func(string, ...string) (string, error) { return out, nil })
	if got != "ABC-123-DEF" {
		t.Fatalf("MachineID = %q", got)
	}
	if got := MachineID(func(string, ...string) (string, error) { return "", os.ErrNotExist }); got != "" {
		t.Fatalf("MachineID on failure = %q, want empty", got)
	}
}
