// Package vl is a thin VictoriaLogs LogsQL client. Responses are NDJSON: one
// JSON object per line, with every field value a string.
package vl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Timeout matches TIMEOUT_MS in vl.js: a hung store must fail the query, not
// the refresh loop.
const Timeout = 10 * time.Second

// Client talks to one VictoriaLogs instance.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: Timeout}
}

// Query runs a LogsQL query against /select/logsql/query and parses the
// NDJSON response into string-valued maps, one per line.
func (c *Client) Query(ctx context.Context, query string, limit int) ([]map[string]string, error) {
	// A timeout scoped to this call, not to the injected *http.Client, so it
	// applies whichever client the caller supplies: vl.js's AbortSignal.timeout
	// binds per fetch, not to a shared client instance.
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	form := url.Values{
		"query": {query},
		"limit": {strconv.Itoa(limit)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/select/logsql/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("VL %d", resp.StatusCode)
	}

	var rows []map[string]string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var row map[string]string
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}
