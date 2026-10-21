package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStdioAllowlistAndTimeout(t *testing.T) {
	// A shell fixture needs no Codex installation, credentials, or network. It
	// intentionally never answers the second RPC, exercising process cleanup.
	home := t.TempDir()
	binary := filepath.Join(home, "fake-codex")
	script := "#!/bin/sh\nread -r init\nprintf '%s\\n' '{\"id\":1,\"result\":{}}'\nread -r initialized\nread -r request\nread -r wait_for_eof\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := Open(context.Background(), Options{Home: home, Binary: binary, RequestTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.(*stdioClient).timeout = 100 * time.Millisecond
	var out struct{}
	for _, method := range []string{"thread/start", "thread/resume", "turn/start", "account/login/start", "config/value/write"} {
		if err := c.Call(context.Background(), method, map[string]any{}, &out); err == nil || !strings.Contains(err.Error(), "not a monitoring read") {
			t.Fatalf("unsafe method accepted: %s: %v", method, err)
		}
	}
	start := time.Now()
	err = c.Call(context.Background(), "thread/list", map[string]any{}, &out)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || time.Since(start) > 2*time.Second {
		t.Fatalf("request not bounded: %v", err)
	}
}

// This opt-in smoke reads only rollout summaries. It neither starts App Server
// nor uses credentials and never logs conversation contents or session IDs.
func TestReadonlyCorpusSmoke(t *testing.T) {
	home := os.Getenv("CODEX_OBS_READONLY_SMOKE_HOME")
	if home == "" {
		t.Skip("opt-in local rollout coverage check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := NewCollector()
	opts := Options{Home: home, ProfileID: "smoke", LocalOnly: true}
	r, err := c.Collect(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	known, priced, requests := 0, 0, 0
	for _, s := range r.Sessions {
		if s.UsageKnown {
			known++
		}
		if s.CostKnown {
			priced++
		}
		requests += s.Requests
	}
	t.Logf("sessions=%d usage_available=%d fully_priced=%d requests=%d warning_kinds=%d legacy=%v paginated=%v", len(r.Sessions), known, priced, requests, len(r.Warnings), r.Capabilities.LegacyHistory, r.Capabilities.PaginatedHistory)
	start := time.Now()
	again, err := c.Collect(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Sessions) < len(r.Sessions) {
		t.Fatal("unchanged rescan dropped sessions")
	}
	t.Logf("incremental rescan=%s", time.Since(start))
}
