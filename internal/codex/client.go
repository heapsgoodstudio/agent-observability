package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// The allowlist is deliberate: a monitoring transport cannot accidentally
// execute inference, resume a thread, refresh credentials, or change settings.
var readMethods = map[string]bool{
	"initialize": true, "thread/list": true, "thread/read": true,
	"thread/turns/list": true, "thread/items/list": true,
	"account/rateLimits/read": true, "model/list": true,
}

type RPCError struct {
	Code   int
	Method string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("codex: %s returned RPC error %d", e.Method, e.Code)
}
func unsupported(err error) bool {
	var e *RPCError
	return errors.As(err, &e) && (e.Code == -32601 || e.Code == -32602)
}

type envelope struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}
type stdioClient struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	in        io.WriteCloser
	responses chan envelope
	done      chan struct{}
	cancel    context.CancelFunc
	cleanup   string
	timeout   time.Duration
	next      int64
	once      sync.Once
}

// Open starts Codex's documented stdio App Server. Only read operations are
// issued. Codex itself may maintain its runtime databases on startup; the
// adapter never edits its config, auth file, transcripts, or private databases.
// https://learn.chatgpt.com/docs/app-server
func Open(ctx context.Context, opts Options) (RPC, error) {
	if opts.Binary == "" {
		opts.Binary = "codex"
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 15 * time.Second
	}
	dir, err := os.MkdirTemp("", "agent-obs-codex-")
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(child, opts.Binary, "app-server", "-c", "analytics.enabled=false", "-c", "otel.exporter=\"none\"", "-c", "otel.metrics_exporter=\"none\"")
	cmd.Dir = dir // never inherit a project's Codex config or trust context
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CODEX_HOME=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+opts.Home)
	cmd.Stderr = io.Discard // may contain prompts, paths, or credential diagnostics
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("codex: start App Server: %w", err)
	}
	c := &stdioClient{cmd: cmd, in: in, responses: make(chan envelope), done: make(chan struct{}), cancel: cancel, cleanup: dir, timeout: opts.RequestTimeout}
	go func() {
		defer close(c.responses)
		s := bufio.NewScanner(out)
		s.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for s.Scan() {
			var e envelope
			if json.Unmarshal(s.Bytes(), &e) != nil || e.ID == 0 {
				continue
			}
			select {
			case c.responses <- e:
			case <-child.Done():
				return
			}
		}
	}()
	go func() { _ = cmd.Wait(); close(c.done) }()
	var initialized struct{}
	err = c.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "agent_observability", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}, &initialized)
	if err != nil {
		c.Close()
		return nil, err
	}
	if _, err := io.WriteString(in, "{\"method\":\"initialized\"}\n"); err != nil {
		c.Close()
		return nil, fmt.Errorf("codex: initialize notification failed")
	}
	return c, nil
}

func (c *stdioClient) Call(ctx context.Context, method string, params, result any) error {
	if !readMethods[method] {
		return fmt.Errorf("codex: method is not a monitoring read")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	c.next++
	id := c.next
	b, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("codex: invalid request")
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("codex: App Server transport closed")
	}
	for {
		select {
		case <-ctx.Done():
			c.cancel()
			return fmt.Errorf("codex: %s: %w", method, ctx.Err())
		case e, ok := <-c.responses:
			if !ok {
				return fmt.Errorf("codex: App Server transport closed")
			}
			if e.ID != id {
				continue
			}
			if e.Error != nil {
				return &RPCError{Code: e.Error.Code, Method: method}
			}
			if err := json.Unmarshal(e.Result, result); err != nil {
				return fmt.Errorf("codex: invalid %s response", method)
			}
			return nil
		}
	}
}

func (c *stdioClient) Close() error {
	c.once.Do(func() { c.cancel(); _ = c.in.Close(); <-c.done; _ = os.RemoveAll(c.cleanup) })
	return nil
}

func connect(ctx context.Context, opts Options) (RPC, error) {
	if opts.Connect != nil {
		return opts.Connect(ctx, opts)
	}
	return Open(ctx, opts)
}

func ReadUsage(ctx context.Context, opts Options) (Usage, error) {
	var result Usage
	if opts.Home == "" {
		return result, fmt.Errorf("codex: home is required")
	}
	c, err := connect(ctx, opts)
	if err != nil {
		return result, err
	}
	defer c.Close()
	type rawWindow struct {
		UsedPercent   *float64 `json:"usedPercent"`
		WindowMinutes *int64   `json:"windowDurationMins"`
		ResetsAt      *int64   `json:"resetsAt"`
	}
	type rawBucket struct {
		Bucket
		Primary   *rawWindow `json:"primary"`
		Secondary *rawWindow `json:"secondary"`
	}
	var response struct {
		RateLimits           *rawBucket           `json:"rateLimits"`
		ByID                 map[string]rawBucket `json:"rateLimitsByLimitId"`
		AccountID            string               `json:"accountId"`
		OrdinaryUsageAllowed *bool                `json:"ordinaryUsageAllowed"`
		ResetCredits         *struct {
			AvailableCount *int64 `json:"availableCount"`
		} `json:"rateLimitResetCredits"`
	}
	// Background monitoring only needs the count, not a separate reset-credit
	// detail lookup. Never advertise automatic reserve/fallback capabilities:
	// doing so can record experiment exposure on the backend.
	err = c.Call(ctx, "account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true}, &response)
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) && rpcErr.Code == -32602 {
		// Older App Servers may reject the new optional polling parameter.
		err = c.Call(ctx, "account/rateLimits/read", map[string]any{}, &response)
	}
	if err != nil {
		return result, err
	}
	result.OrdinaryUsageAllowed = response.OrdinaryUsageAllowed
	if response.ResetCredits != nil && response.ResetCredits.AvailableCount != nil && *response.ResetCredits.AvailableCount >= 0 {
		result.ResetCreditsAvailable = response.ResetCredits.AvailableCount
	}
	usable := result.OrdinaryUsageAllowed != nil || result.ResetCreditsAvailable != nil
	buckets := response.ByID
	if len(buckets) == 0 && response.RateLimits != nil {
		buckets = map[string]rawBucket{response.RateLimits.LimitID: *response.RateLimits}
	}
	plans := map[string]bool{}
	for id, b := range buckets {
		if b.PlanType != "" {
			plans[b.PlanType] = true
		}
		bucket := b.Bucket
		bucket.LimitID = id // multi-bucket map key is the metered identity
		for _, entry := range []struct {
			kind string
			w    *rawWindow
		}{{"primary", b.Primary}, {"secondary", b.Secondary}} {
			if entry.w == nil || entry.w.UsedPercent == nil {
				continue
			}
			w := entry.w
			if *w.UsedPercent < 0 {
				continue
			}
			quota := &QuotaWindow{UsedPercent: *w.UsedPercent, WindowMinutes: w.WindowMinutes, ResetsAt: w.ResetsAt}
			if entry.kind == "primary" {
				bucket.Primary = quota
			} else {
				bucket.Secondary = quota
			}
			window := Window{LimitID: id, Name: b.Name, Kind: entry.kind, UsedPercent: *w.UsedPercent}
			if w.WindowMinutes != nil {
				window.WindowMinutes = *w.WindowMinutes
				window.WindowMinutesKnown = true
			}
			if w.ResetsAt != nil {
				window.ResetsAt = *w.ResetsAt
				window.ResetsAtKnown = true
			}
			result.Windows = append(result.Windows, window)
		}
		usable = usable || bucket.Primary != nil || bucket.Secondary != nil || bucket.Credits != nil || bucket.IndividualLimit != nil || bucket.SpendControlReached != nil || bucket.RateLimitReachedType != nil
		result.Buckets = append(result.Buckets, bucket)
	}
	// A single compatible plan is useful; inconsistent bucket plans must not
	// make the account label depend on randomized Go map iteration order.
	if len(plans) == 1 {
		for plan := range plans {
			result.PlanType = plan
		}
	} else if len(plans) == 0 && response.RateLimits != nil {
		result.PlanType = response.RateLimits.PlanType
	}
	if !usable {
		return Usage{}, fmt.Errorf("codex: account rate limits are unavailable")
	}
	result.AccountID = response.AccountID
	result.FetchedAt = time.Now().UTC()
	sort.Slice(result.Buckets, func(i, j int) bool { return result.Buckets[i].LimitID < result.Buckets[j].LimitID })
	sort.Slice(result.Windows, func(i, j int) bool {
		a, b := result.Windows[i], result.Windows[j]
		if a.LimitID != b.LimitID {
			return a.LimitID < b.LimitID
		}
		return a.Kind < b.Kind
	})
	return result, nil
}
