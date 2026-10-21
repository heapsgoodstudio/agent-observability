package vm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQParsesInstantResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %q, want /api/v1/query", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != "up" {
			t.Errorf("query param = %q, want up", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"source":"cache"},"value":[1755200000,"42.5"]}]}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	res, err := c.Q(context.Background(), "up")
	if err != nil {
		t.Fatalf("Q() error = %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("len(res) = %d, want 1", len(res))
	}
	if res[0].Metric["source"] != "cache" {
		t.Errorf("Metric[source] = %q, want cache", res[0].Metric["source"])
	}
	v, ok := Scalar(res)
	if !ok || v != 42.5 {
		t.Errorf("Scalar() = (%v, %v), want (42.5, true)", v, ok)
	}
}

func TestQRParsesRangeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path = %q, want /api/v1/query_range", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("start") != "1755200000" || q.Get("end") != "1755200060" || q.Get("step") != "60s" {
			t.Errorf("range params = %v", q)
		}
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"values":[[1755200000,"1"],[1755200060,"2"]]}]}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	res, err := c.QR(context.Background(), "up", 1755200000, 1755200060, "60s")
	if err != nil {
		t.Fatalf("QR() error = %v", err)
	}
	pts := Points(res)
	if len(pts) != 2 {
		t.Fatalf("len(pts) = %d, want 2", len(pts))
	}
	if pts[0].TS != 1755200000 || pts[0].V != 1 {
		t.Errorf("pts[0] = %+v, want {1755200000 1}", pts[0])
	}
	if pts[1].TS != 1755200060 || pts[1].V != 2 {
		t.Errorf("pts[1] = %+v, want {1755200060 2}", pts[1])
	}
}

func TestQErrorStatusCarriesBodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error","error":"boom"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.Q(context.Background(), "up")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want containing %q", err, "boom")
	}
}

func TestQHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.Q(context.Background(), "up"); err == nil {
		t.Fatal("want error for 500 response")
	}
}

func TestScalarAbsentOrNaN(t *testing.T) {
	if _, ok := Scalar(nil); ok {
		t.Error("Scalar(nil) ok = true, want false")
	}
	if _, ok := Scalar([]Series{{Value: nil}}); ok {
		t.Error("Scalar with no Value ok = true, want false")
	}
	if _, ok := Scalar([]Series{{Value: []any{1755200000.0, "NaN"}}}); ok {
		t.Error("Scalar with NaN value ok = true, want false")
	}
}
