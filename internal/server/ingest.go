package server

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Codex exporters do not need to support custom resource attributes: an
// installation-owned endpoint assigns profile/provider before forwarding OTLP
// to the existing stores. This route never relays to a user-supplied URL.
func codexIngest(o Options) http.Handler {
	client := &http.Client{Timeout: 10 * time.Second}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST required", 405)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			http.Error(w, "browser telemetry requests are not accepted", 403)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/ingest/codex/"), "/")
		if len(parts) != 3 || parts[1] != "v1" || (parts[2] != "logs" && parts[2] != "metrics") || o.Profiles == nil {
			http.NotFound(w, r)
			return
		}
		ps, err := o.Profiles.List()
		if err != nil {
			http.Error(w, "profile settings unavailable", 503)
			return
		}
		allowed := false
		for _, p := range ps {
			if p.ID == parts[0] && p.Provider == profiles.Codex && p.Enabled {
				allowed = true
			}
		}
		if !allowed {
			http.Error(w, "profile is not enabled", 404)
			return
		}
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || (contentType != "application/x-protobuf" && contentType != "application/json") {
			http.Error(w, "OTLP protobuf or JSON required", 415)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		defer r.Body.Close()
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "invalid gzip", 400)
				return
			}
			defer gz.Close()
			reader = gz
		} else if r.Header.Get("Content-Encoding") != "" {
			http.Error(w, "unsupported encoding", 415)
			return
		}
		body, err := io.ReadAll(io.LimitReader(reader, (32<<20)+1))
		if err != nil || len(body) > 32<<20 {
			http.Error(w, "telemetry payload too large or unreadable", 413)
			return
		}
		var msg proto.Message
		if parts[2] == "logs" {
			msg = &logspb.LogsData{}
		} else {
			msg = &metricspb.MetricsData{}
		}
		if contentType == "application/json" {
			err = protojson.Unmarshal(body, msg)
		} else {
			err = proto.Unmarshal(body, msg)
		}
		if err != nil {
			http.Error(w, "invalid OTLP payload", 400)
			return
		}
		switch data := msg.(type) {
		case *logspb.LogsData:
			for _, batch := range data.ResourceLogs {
				batch.Resource = tagResource(batch.Resource, parts[0])
			}
		case *metricspb.MetricsData:
			for _, batch := range data.ResourceMetrics {
				batch.Resource = tagResource(batch.Resource, parts[0])
			}
		}
		body, err = proto.Marshal(msg)
		if err != nil {
			http.Error(w, "encode telemetry", 500)
			return
		}
		base := "http://127.0.0.1:9428"
		endpoint := "/insert/opentelemetry/v1/logs"
		if o.VL != nil {
			base = o.VL.BaseURL
		}
		if parts[2] == "metrics" {
			base = "http://127.0.0.1:8428"
			endpoint = "/opentelemetry/v1/metrics"
			if o.VM != nil {
				base = o.VM.BaseURL
			}
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(base, "/")+endpoint, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "collector unavailable", 502)
			return
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "local telemetry store unavailable", 503)
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && o.DB != nil {
			db.MetaSet(o.DB, "codex.telemetry."+parts[0], fmt.Sprint(time.Now().Unix()))
		}
	})
}

func tagResource(r *resourcepb.Resource, id string) *resourcepb.Resource {
	if r == nil {
		r = &resourcepb.Resource{}
	}
	attrs := r.Attributes[:0]
	for _, a := range r.Attributes {
		if a != nil && a.Key != "profile" && a.Key != "provider" {
			attrs = append(attrs, a)
		}
	}
	for k, v := range map[string]string{"profile": id, "provider": "codex"} {
		attrs = append(attrs, &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}})
	}
	r.Attributes = attrs
	return r
}
