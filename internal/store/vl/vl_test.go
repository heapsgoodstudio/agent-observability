package vl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueryParsesNDJSONAndPostsForm(t *testing.T) {
	var gotMethod, gotQuery, gotLimit, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotQuery = r.FormValue("query")
		gotLimit = r.FormValue("limit")
		w.Write([]byte("{\"_time\":\"2026-08-15T00:00:00Z\",\"level\":\"info\"}\n{\"_time\":\"2026-08-15T00:00:01Z\",\"level\":\"warn\"}\n"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	rows, err := c.Query(context.Background(), `level:"info"`, 100)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.HasPrefix(gotContentType, "application/x-www-form-urlencoded") {
		t.Errorf("content-type = %q, want form-urlencoded", gotContentType)
	}
	if gotQuery != `level:"info"` {
		t.Errorf("query form value = %q", gotQuery)
	}
	if gotLimit != "100" {
		t.Errorf("limit form value = %q, want 100", gotLimit)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0]["level"] != "info" || rows[0]["_time"] != "2026-08-15T00:00:00Z" {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1]["level"] != "warn" {
		t.Errorf("rows[1] = %+v", rows[1])
	}
}

func TestQueryHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.Query(context.Background(), "up", 10); err == nil {
		t.Fatal("want error for 500 response")
	}
}
