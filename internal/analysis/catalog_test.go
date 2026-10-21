package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestModelCatalogParsers(t *testing.T) {
	for _, tc := range []struct {
		provider, raw string
		want          []ModelOption
	}{
		{"claude", `{"type":"control_response","response":{"request_id":"unrelated","subtype":"success","response":{"models":[{"value":"wrong"}]}}}
{"type":"control_response","response":{"request_id":"model-discovery","subtype":"success","response":{"models":[{"value":"default","displayName":"Recommended"},{"value":"opus[1m]","displayName":"Opus"}]}}}`, []ModelOption{{ID: "opus[1m]", Name: "Opus"}}},
		{"omp", `{"models":[{"provider":"vendor","id":"org/model","selector":"vendor/org/model","name":"Model","kind":"chat","apiKey":"must-not-escape"},{"provider":"vendor","id":"embed","kind":"embedding"}]}`, []ModelOption{{ID: "vendor/org/model", Name: "Model", Provider: "vendor"}}},
		{"opencode", "vendor/org/model\nvendor/org/model\ninvalid words/ignored\n", []ModelOption{{ID: "vendor/org/model", Name: "org/model", Provider: "vendor"}}},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			got, err := parseModels(tc.provider, []byte(tc.raw))
			if err != nil || !reflect.DeepEqual(normalizeModels(got), tc.want) {
				t.Fatalf("models=%+v error=%v", got, err)
			}
		})
	}
	for _, provider := range []string{"claude", "omp", "opencode"} {
		if _, err := parseModels(provider, []byte(`{"error":"not a catalog"}`)); err == nil {
			t.Errorf("%s accepted an error as a catalog", provider)
		}
	}
}

func TestModelCatalogCoalescesRequestsAndKeepsHomesSeparate(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c := ModelCatalog{Discover: func(ctx context.Context, target ModelTarget) ([]ModelOption, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return []ModelOption{{ID: target.Directory, Name: target.Provider}}, nil
	}}
	target := ModelTarget{Provider: "codex", Directory: "one", Binary: "cli"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.List(context.Background(), target)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	models, err := c.List(context.Background(), target)
	if err != nil || calls.Load() != 1 || models[0].ID != "one" {
		t.Fatalf("cache=%+v calls=%d err=%v", models, calls.Load(), err)
	}
	models[0].ID = "mutated"
	again, _ := c.List(context.Background(), target)
	if again[0].ID != "one" {
		t.Fatal("caller mutated cached catalog")
	}
	target.Directory = "two"
	models, err = c.List(context.Background(), target)
	if err != nil || calls.Load() != 2 || models[0].ID != "two" {
		t.Fatalf("homes mixed: %+v %v", models, err)
	}
}

func TestModelCatalogCachesFailures(t *testing.T) {
	var calls int
	c := ModelCatalog{Discover: func(context.Context, ModelTarget) ([]ModelOption, error) {
		calls++
		return nil, errors.New("unavailable")
	}}
	for i := 0; i < 2; i++ {
		if _, err := c.List(context.Background(), ModelTarget{}); err == nil {
			t.Fatal("failure hidden")
		}
	}
	if calls != 1 {
		t.Fatalf("failed CLI repeatedly spawned: %d", calls)
	}
}

func TestCatalogCommandsSendMetadataOnlyAndSelectProfile(t *testing.T) {
	for _, provider := range []string{"claude", "omp", "opencode"} {
		t.Run(provider, func(t *testing.T) {
			dir := t.TempDir()
			profile := filepath.Join(dir, "opencode")
			os.Mkdir(profile, 0700)
			argv := filepath.Join(dir, "argv")
			input := filepath.Join(dir, "stdin")
			env := filepath.Join(dir, "selected-home")
			out := map[string]string{"claude": `{"type":"control_response","response":{"request_id":"model-discovery","subtype":"success","response":{"models":[{"value":"sonnet","displayName":"Sonnet"}]}}}`, "omp": `{"models":[{"provider":"vendor","id":"model"}]}`, "opencode": "vendor/model"}[provider]
			key := map[string]string{"claude": "CLAUDE_CONFIG_DIR", "omp": "PI_CODING_AGENT_DIR", "opencode": "XDG_DATA_HOME"}[provider]
			bin := fakeScript(t, dir, "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+argv+"'\ncat > '"+input+"'\nprintf '%s' \"$"+key+"\" > '"+env+"'\nprintf '%s\\n' '"+out+"'\n")
			models, err := discoverModels(context.Background(), ModelTarget{Provider: provider, Directory: profile, Binary: bin})
			if err != nil || len(models) != 1 {
				t.Fatalf("%+v %v", models, err)
			}
			args, _ := os.ReadFile(argv)
			stdin, _ := os.ReadFile(input)
			home, _ := os.ReadFile(env)
			wantHome := profile
			if provider == "opencode" {
				wantHome = dir
			}
			if string(home) != wantHome {
				t.Fatalf("wrong credential home: %s", home)
			}
			if provider == "claude" {
				var request struct {
					Type    string `json:"type"`
					Request struct {
						Subtype string `json:"subtype"`
					} `json:"request"`
				}
				if json.Unmarshal(stdin, &request) != nil || request.Type != "control_request" || request.Request.Subtype != "initialize" {
					t.Fatalf("not a metadata request: %s", stdin)
				}
			} else if len(stdin) != 0 || !strings.HasPrefix(string(args), "models\n") {
				t.Fatalf("not a metadata command: %s", args)
			}
		})
	}
}

type modelRPC struct {
	pages []string
	calls int
}

func (m *modelRPC) Close() error { return nil }
func (m *modelRPC) Call(_ context.Context, method string, params, result any) error {
	if method != "model/list" {
		return errors.New("unexpected non-metadata operation")
	}
	p := params.(map[string]any)
	if p["includeHidden"] != false || (m.calls > 0 && p["cursor"] != "next") {
		return errors.New("invalid paging/filter")
	}
	raw := m.pages[m.calls]
	m.calls++
	return json.Unmarshal([]byte(raw), result)
}
func TestCodexModelsUsesModelSelectorAndPagesVisibleCatalog(t *testing.T) {
	c := &modelRPC{pages: []string{`{"data":[{"id":"catalog-id","model":"model-one","displayName":"One"},{"model":"hidden","hidden":true}],"nextCursor":"next"}`, `{"data":[{"model":"model-two","displayName":"Two"}],"nextCursor":null}`}}
	got, err := codexModels(context.Background(), c)
	if err != nil || len(got) != 2 || got[0].ID != "model-one" || got[1].ID != "model-two" || c.calls != 2 {
		t.Fatalf("%+v %v", got, err)
	}
}
