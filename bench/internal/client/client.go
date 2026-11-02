// Package client はルーター経由で ISUReal を叩く HTTP クライアント。
// 公式ベンチと同じく X-Country-Code でリージョンを選び、X-Client-IP で利用者ごとの接続元を名乗る。
// リダイレクトは追わず、503 だけ再試行する (manual.md「猶予時間」)。再試行してもエラーとして数える。
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"isureal-bench/internal/api"
)

// Session は 1 利用者 (または管理者) の送信コンテキスト。
type Session struct {
	Token    string // Bearer トークン。空なら Authorization を付けない
	Country  string // X-Country-Code。空なら付けない (ルーターは JP 扱い)
	ClientIP string // X-Client-IP
}

type Response struct {
	Status  int
	Header  http.Header
	Body    []byte
	Elapsed time.Duration
}

// Error は 1 リクエストの失敗。公式の文言に寄せたメッセージを String で返す。
type Error struct {
	Label   string // 例 "GET /api/notifications"
	Want    int
	Got     int
	Code    string // JSON の error フィールド
	Err     error  // 通信エラー
	Timeout bool
	// 呼び出し側の ctx が終わっていた (ベンチが打ち切った) 要求
	Canceled bool
}

func (e *Error) Error() string {
	if e.Err != nil {
		if e.Timeout {
			return fmt.Sprintf("%s: タイムアウトしました", e.Label)
		}
		return fmt.Sprintf("%s: リクエストに失敗しました (%v)", e.Label, e.Err)
	}
	code := e.Code
	if code == "" {
		code = "unknown"
	}
	return fmt.Sprintf("%s: ステータスコード %d が返りました（期待値 %d、%s）", e.Label, e.Got, e.Want, code)
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
	Timeout time.Duration
	Stats   *Stats
	// 503 の再試行回数
	Retries503 int
}

func New(baseURL string, timeout time.Duration) *Client {
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 4096,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   false,
		DisableCompression:  true,
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP: &http.Client{
			Transport: transport,
			// ベンチマーカーはリダイレクトを追わない
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Timeout:    timeout,
		Stats:      NewStats(),
		Retries503: 2,
	}
}

func (c *Client) newRequest(ctx context.Context, sess *Session, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		if sess.Token != "" {
			req.Header.Set("Authorization", "Bearer "+sess.Token)
		}
		if sess.Country != "" {
			req.Header.Set("X-Country-Code", sess.Country)
		}
		if sess.ClientIP != "" {
			req.Header.Set("X-Client-IP", sess.ClientIP)
		}
	}
	req.Header.Set("User-Agent", "isureal-bench/1.0")
	return req, nil
}

// Do は 1 リクエストを送り、ステータスが want と違えば *Error を返す (本文は返す)。
// label は統計のキー (例 "GET /api/posts/:id")。
func (c *Client) Do(ctx context.Context, sess *Session, method, path, label string, body []byte, contentType string, want int) (*Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, sess, method, path, label, body, contentType, want)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var e *Error
		if errors.As(err, &e) && e.Got == http.StatusServiceUnavailable && attempt < c.Retries503 && ctx.Err() == nil {
			select {
			case <-time.After(time.Duration(100*(attempt+1)) * time.Millisecond):
			case <-ctx.Done():
				return resp, lastErr
			}
			continue
		}
		return resp, lastErr
	}
}

func (c *Client) once(parent context.Context, sess *Session, method, path, label string, body []byte, contentType string, want int) (*Response, error) {
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := c.newRequest(ctx, sess, method, path, reader)
	if err != nil {
		return nil, &Error{Label: label, Want: want, Err: err}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	start := time.Now()
	res, err := c.HTTP.Do(req)
	if err != nil {
		elapsed := time.Since(start)
		canceled := parent.Err() != nil
		timeout := !canceled && (errors.Is(err, context.DeadlineExceeded) || isTimeout(err))
		if !canceled { // ベンチが打ち切った要求は統計に入れない
			c.Stats.record(label, 0, elapsed, true)
		}
		return nil, &Error{Label: label, Want: want, Err: err, Timeout: timeout, Canceled: canceled}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	elapsed := time.Since(start)
	if err != nil {
		canceled := parent.Err() != nil
		timeout := !canceled && (errors.Is(err, context.DeadlineExceeded) || isTimeout(err))
		if !canceled {
			c.Stats.record(label, res.StatusCode, elapsed, true)
		}
		return nil, &Error{Label: label, Want: want, Got: res.StatusCode, Err: err, Timeout: timeout, Canceled: canceled}
	}
	resp := &Response{Status: res.StatusCode, Header: res.Header, Body: data, Elapsed: elapsed}
	ok := want == 0 || res.StatusCode == want
	c.Stats.record(label, res.StatusCode, elapsed, !ok)
	if !ok {
		var body api.ErrorResponse
		_ = json.Unmarshal(data, &body)
		return resp, &Error{Label: label, Want: want, Got: res.StatusCode, Code: body.Error}
	}
	return resp, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// JSON は JSON を送って JSON を受け取る。
func (c *Client) JSON(ctx context.Context, sess *Session, method, path, label string, in any, want int, out any) (*Response, error) {
	var body []byte
	contentType := ""
	if in != nil {
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			return nil, err
		}
		contentType = "application/json"
	}
	resp, err := c.Do(ctx, sess, method, path, label, body, contentType, want)
	if err != nil {
		return resp, err
	}
	if out != nil {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return resp, &Error{Label: label, Want: want, Got: resp.Status, Code: "invalid_json", Err: fmt.Errorf("JSON を解釈できません: %w", err)}
		}
	}
	return resp, nil
}

// Notification は SSE を購読して最初の通知を返す。ctx が切れるか接続が閉じれば終わる。
func (c *Client) Notification(ctx context.Context, sess *Session) (*api.Notification, error) {
	const label = "GET /api/notifications"
	req, err := c.newRequest(ctx, sess, http.MethodGet, "/api/notifications", nil)
	if err != nil {
		return nil, &Error{Label: label, Want: 200, Err: err}
	}
	req.Header.Set("Accept", "text/event-stream")
	start := time.Now()
	res, err := c.HTTP.Do(req)
	if err != nil {
		canceled := ctx.Err() != nil
		if !canceled {
			c.Stats.record(label, 0, time.Since(start), true)
		}
		return nil, &Error{Label: label, Want: 200, Err: err, Timeout: !canceled && errors.Is(err, context.DeadlineExceeded), Canceled: canceled}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		var body api.ErrorResponse
		_ = json.Unmarshal(data, &body)
		c.Stats.record(label, res.StatusCode, time.Since(start), true)
		return nil, &Error{Label: label, Want: 200, Got: res.StatusCode, Code: body.Error}
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		c.Stats.record(label, res.StatusCode, time.Since(start), true)
		return nil, &Error{Label: label, Want: 200, Got: res.StatusCode, Code: "not_event_stream", Err: fmt.Errorf("Content-Type が text/event-stream ではありません: %q", ct)}
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				var n api.Notification
				if err := json.Unmarshal([]byte(data.String()), &n); err != nil {
					c.Stats.record(label, res.StatusCode, time.Since(start), true)
					return nil, &Error{Label: label, Want: 200, Got: res.StatusCode, Code: "invalid_event", Err: fmt.Errorf("通知の JSON を解釈できません: %w", err)}
				}
				c.Stats.record(label, res.StatusCode, time.Since(start), false)
				return &n, nil
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	err = scanner.Err()
	if ctx.Err() != nil {
		return nil, &Error{Label: label, Want: 200, Err: ctx.Err(), Timeout: true, Canceled: true}
	}
	c.Stats.record(label, res.StatusCode, time.Since(start), true)
	if err == nil {
		err = errors.New("通知が届く前に接続が閉じました")
	}
	return nil, &Error{Label: label, Want: 200, Got: res.StatusCode, Err: err}
}

// ---- 統計 ----

type EndpointStats struct {
	Count     int64
	Errors    int64
	Statuses  map[int]int64
	Durations []time.Duration
}

type Stats struct {
	mu      sync.Mutex
	byLabel map[string]*EndpointStats
}

func NewStats() *Stats { return &Stats{byLabel: map[string]*EndpointStats{}} }

func (s *Stats) record(label string, status int, d time.Duration, isErr bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.byLabel[label]
	if e == nil {
		e = &EndpointStats{Statuses: map[int]int64{}}
		s.byLabel[label] = e
	}
	e.Count++
	e.Statuses[status]++
	if isErr {
		e.Errors++
	}
	e.Durations = append(e.Durations, d)
}

type EndpointSummary struct {
	Label    string           `json:"label"`
	Count    int64            `json:"count"`
	Errors   int64            `json:"errors"`
	Statuses map[string]int64 `json:"statuses"`
	MeanMs   float64          `json:"mean_ms"`
	P50Ms    float64          `json:"p50_ms"`
	P95Ms    float64          `json:"p95_ms"`
	MaxMs    float64          `json:"max_ms"`
}

// Summary は alp 風の表 (件数順)。
func (s *Stats) Summary() []EndpointSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EndpointSummary, 0, len(s.byLabel))
	for label, e := range s.byLabel {
		ds := append([]time.Duration(nil), e.Durations...)
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		var sum time.Duration
		for _, d := range ds {
			sum += d
		}
		pct := func(p float64) float64 {
			if len(ds) == 0 {
				return 0
			}
			i := int(float64(len(ds)-1) * p)
			return float64(ds[i]) / float64(time.Millisecond)
		}
		statuses := map[string]int64{}
		for st, n := range e.Statuses {
			statuses[fmt.Sprint(st)] = n
		}
		var mean float64
		if len(ds) > 0 {
			mean = float64(sum) / float64(len(ds)) / float64(time.Millisecond)
		}
		out = append(out, EndpointSummary{Label: label, Count: e.Count, Errors: e.Errors, Statuses: statuses, MeanMs: mean, P50Ms: pct(0.5), P95Ms: pct(0.95), MaxMs: pct(1)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func (s *Stats) Totals() (count, errs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.byLabel {
		count += e.Count
		errs += e.Errors
	}
	return
}
