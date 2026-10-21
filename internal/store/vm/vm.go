// Package vm is a thin VictoriaMetrics PromQL client. It carries no dedupe or
// caching of its own; that lives in store.Memo, one layer up.
package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Timeout matches TIMEOUT_MS in vm.js: a hung store must fail the query, not
// hang the caller forever.
const Timeout = 10 * time.Second

// Client talks to one VictoriaMetrics instance.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// Series mirrors one VM result entry's JSON shape. Value carries the instant
// query's [ts, "value"] pair; Values carries the range query's list of the
// same pairs. Both are []any because VM encodes the timestamp as a JSON
// number and the value as a JSON string.
type Series struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value,omitempty"`
	Values [][2]any          `json:"values,omitempty"`
}

// Point is one (timestamp, value) pair extracted from a Series' Values.
type Point struct {
	TS float64
	V  float64
}

type queryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		Result []Series `json:"result"`
	} `json:"data"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: Timeout}
}

func (c *Client) call(ctx context.Context, path string, params url.Values) ([]Series, error) {
	// A timeout scoped to this call, not to the injected *http.Client, so it
	// applies whichever client the caller supplies: vm.js's AbortSignal.timeout
	// binds per fetch, not to a shared client instance.
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	reqURL := fmt.Sprintf("%s%s?%s", c.BaseURL, path, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("VM %d", resp.StatusCode)
	}
	var body queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		msg := body.Error
		if msg == "" {
			msg = "query failed"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return body.Data.Result, nil
}

// Q runs an instant query against /api/v1/query.
func (c *Client) Q(ctx context.Context, query string) ([]Series, error) {
	return c.call(ctx, "/api/v1/query", url.Values{"query": {query}})
}

// QR runs a range query against /api/v1/query_range.
func (c *Client) QR(ctx context.Context, query string, start, end int64, step string) ([]Series, error) {
	return c.call(ctx, "/api/v1/query_range", url.Values{
		"query": {query},
		"start": {strconv.FormatInt(start, 10)},
		"end":   {strconv.FormatInt(end, 10)},
		"step":  {step},
	})
}

// toFloat reads a JSON-decoded VM value, which arrives as either a float64
// (a timestamp) or a string (a sample value formatted by VM).
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// Scalar reads the first series' instant value. It reports false when no
// series was returned or the value is absent or NaN.
func Scalar(res []Series) (float64, bool) {
	if len(res) == 0 || len(res[0].Value) < 2 {
		return 0, false
	}
	f, ok := toFloat(res[0].Value[1])
	if !ok || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// SeriesValue reads one series' instant value. Scalar answers "the value" for
// a query reduced to a single series; this answers "this series' value" for a
// query grouped by a label, where every series matters and the first one is
// not special.
func SeriesValue(s Series) (float64, bool) {
	if len(s.Value) < 2 {
		return 0, false
	}
	f, ok := toFloat(s.Value[1])
	if !ok || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// Points reads the first series' range values as a slice of Point. A value
// that fails to parse is skipped rather than aborting the whole series.
func Points(res []Series) []Point {
	if len(res) == 0 {
		return nil
	}
	pts := make([]Point, 0, len(res[0].Values))
	for _, v := range res[0].Values {
		ts, tsOK := toFloat(v[0])
		val, valOK := toFloat(v[1])
		if !tsOK || !valOK {
			continue
		}
		pts = append(pts, Point{TS: ts, V: val})
	}
	return pts
}
