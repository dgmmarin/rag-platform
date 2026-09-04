package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// Pagination and retry ceilings.
const (
	// defaultMaxPages bounds every pagination loop so a broken API that never signals
	// the end cannot spin forever (ponytail: hard ceiling; a source can lower it via
	// pagination.max_pages, and the upgrade path is a per-source override if a real API
	// legitimately exceeds it).
	defaultMaxPages  = 10000
	defaultPageSize  = 100
	defaultStartPage = 1

	// Retry-After / rate-limit handling.
	maxRetries = 5
	// maxBackoff caps how long we honour a Retry-After, so a hostile/huge Retry-After
	// cannot stall a sync indefinitely (ponytail: ceiling; upgrade path is a
	// configurable per-source cap).
	maxBackoff = 60 * time.Second
)

// apiClient performs authenticated, rate-limited, SSRF-guarded fetches and walks an
// endpoint's pagination.
type apiClient struct {
	baseURL string
	ac      authClient
	limiter *rate.Limiter
	log     *slog.Logger
}

func newClient(baseURL string, ac authClient, limiter *rate.Limiter, log *slog.Logger) *apiClient {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &apiClient{baseURL: strings.TrimRight(baseURL, "/"), ac: ac, limiter: limiter, log: log}
}

// enumerate walks every page of ep and calls emit once per record. It returns the
// total bytes fetched. Every strategy terminates safely at a max-pages ceiling.
func (c *apiClient) enumerate(ctx context.Context, ep endpoint, emit func(item any) error) (int64, error) {
	method := ep.Method
	if method == "" {
		method = http.MethodGet
	}
	maxPages := ep.Pagination.MaxPages
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}

	switch ep.Pagination.Type {
	case "", "none":
		return c.pageOnce(ctx, method, ep, emit)
	case "page":
		return c.pageNumbered(ctx, method, ep, maxPages, emit)
	case "offset":
		return c.pageOffset(ctx, method, ep, maxPages, emit)
	case "cursor":
		return c.pageCursor(ctx, method, ep, maxPages, emit)
	case "link-header":
		return c.pageLinkHeader(ctx, method, ep, maxPages, emit)
	default:
		return 0, fmt.Errorf("api: unsupported pagination type %q", ep.Pagination.Type)
	}
}

// emitItems resolves ep.ItemsPath to the page's records and emits each; it returns the
// number emitted so callers can detect an empty/short final page.
func emitItems(root any, itemsPath string, emit func(item any) error) (int, error) {
	items, ok := evalItems(root, itemsPath)
	if !ok {
		return 0, nil // no items array => treat as an empty (final) page
	}
	for _, it := range items {
		if err := emit(it); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}

func (c *apiClient) pageOnce(ctx context.Context, method string, ep endpoint, emit func(item any) error) (int64, error) {
	root, _, n, err := c.getJSON(ctx, method, c.baseURL+ep.Path)
	if err != nil {
		return n, err
	}
	_, err = emitItems(root, ep.ItemsPath, emit)
	return n, err
}

func (c *apiClient) pageNumbered(ctx context.Context, method string, ep endpoint, maxPages int, emit func(item any) error) (int64, error) {
	p := ep.Pagination
	size := p.Size
	page := p.StartPage
	if page <= 0 {
		page = defaultStartPage
	}
	var total int64
	for i := 0; i < maxPages; i++ {
		q := url.Values{}
		q.Set(paramOr(p.PageParam, "page"), strconv.Itoa(page))
		if size > 0 {
			q.Set(paramOr(p.SizeParam, "per_page"), strconv.Itoa(size))
		}
		root, _, n, err := c.getJSON(ctx, method, withQuery(c.baseURL+ep.Path, q))
		total += n
		if err != nil {
			return total, err
		}
		got, err := emitItems(root, ep.ItemsPath, emit)
		if err != nil {
			return total, err
		}
		if got == 0 || (size > 0 && got < size) { // empty or short page = last
			return total, nil
		}
		page++
	}
	return total, nil
}

func (c *apiClient) pageOffset(ctx context.Context, method string, ep endpoint, maxPages int, emit func(item any) error) (int64, error) {
	p := ep.Pagination
	limit := p.Size
	if limit <= 0 {
		limit = defaultPageSize
	}
	offset := 0
	var total int64
	for i := 0; i < maxPages; i++ {
		q := url.Values{}
		q.Set(paramOr(p.OffsetParam, "offset"), strconv.Itoa(offset))
		q.Set(paramOr(p.LimitParam, "limit"), strconv.Itoa(limit))
		root, _, n, err := c.getJSON(ctx, method, withQuery(c.baseURL+ep.Path, q))
		total += n
		if err != nil {
			return total, err
		}
		got, err := emitItems(root, ep.ItemsPath, emit)
		if err != nil {
			return total, err
		}
		if got == 0 || got < limit { // short/empty page = exhausted
			return total, nil
		}
		offset += limit
	}
	return total, nil
}

func (c *apiClient) pageCursor(ctx context.Context, method string, ep endpoint, maxPages int, emit func(item any) error) (int64, error) {
	p := ep.Pagination
	cursor := ""
	var total int64
	for i := 0; i < maxPages; i++ {
		u := c.baseURL + ep.Path
		if cursor != "" {
			q := url.Values{}
			q.Set(paramOr(p.CursorParam, "cursor"), cursor)
			u = withQuery(u, q)
		}
		root, _, n, err := c.getJSON(ctx, method, u)
		total += n
		if err != nil {
			return total, err
		}
		if _, err := emitItems(root, ep.ItemsPath, emit); err != nil {
			return total, err
		}
		next, ok := evalString(root, p.CursorPath)
		if !ok || next == "" { // no next cursor = done
			return total, nil
		}
		cursor = next
	}
	return total, nil
}

func (c *apiClient) pageLinkHeader(ctx context.Context, method string, ep endpoint, maxPages int, emit func(item any) error) (int64, error) {
	u := c.baseURL + ep.Path
	var total int64
	for i := 0; i < maxPages; i++ {
		root, hdr, n, err := c.getJSON(ctx, method, u)
		total += n
		if err != nil {
			return total, err
		}
		if _, err := emitItems(root, ep.ItemsPath, emit); err != nil {
			return total, err
		}
		next := nextLink(hdr.Get("Link"))
		if next == "" {
			return total, nil
		}
		// RFC 5988 next may be relative; resolve it against the current URL.
		abs, err := resolveRef(u, next)
		if err != nil {
			return total, fmt.Errorf("api: bad Link next URL %q: %w", next, err)
		}
		u = abs
	}
	return total, nil
}

// getJSON performs one request with rate-limiting and Retry-After handling, then
// decodes the JSON body. It retries a 429/503-with-Retry-After up to maxRetries; the
// single-attempt work (and body close) lives in fetchOnce.
func (c *apiClient) getJSON(ctx context.Context, method, rawURL string) (any, http.Header, int64, error) {
	for attempt := 0; ; attempt++ {
		if c.limiter != nil {
			if err := c.limiter.Wait(ctx); err != nil {
				return nil, nil, 0, err
			}
		}
		root, hdr, n, retryIn, retry, err := c.fetchOnce(ctx, method, rawURL)
		if err != nil {
			return nil, nil, n, err
		}
		if retry {
			if attempt >= maxRetries {
				return nil, nil, 0, fmt.Errorf("api: rate-limited by %s after %d attempts", redactURL(rawURL), attempt+1)
			}
			if retryIn > maxBackoff {
				retryIn = maxBackoff
			}
			c.log.Info("api: honouring Retry-After", "delay", retryIn.String(), "attempt", attempt+1)
			if err := sleepCtx(ctx, retryIn); err != nil {
				return nil, nil, 0, err
			}
			continue
		}
		return root, hdr, n, nil
	}
}

// fetchOnce performs a single HTTP attempt and decodes the JSON body (bounded to
// maxResponseBytes; UseNumber so numeric cursors/ids keep exact text). It closes the
// response body before returning. retry is true (with retryIn) only for a 429/503 that
// carried a parseable Retry-After — the caller decides whether to back off and retry.
func (c *apiClient) fetchOnce(ctx context.Context, method, rawURL string) (root any, hdr http.Header, n int64, retryIn time.Duration, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, nil, 0, 0, false, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.ac.do(req)
	if err != nil {
		return nil, nil, 0, 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		if d, ok := retryAfter(resp.Header); ok {
			return nil, nil, 0, d, true, nil
		}
		return nil, nil, 0, 0, false, fmt.Errorf("api: %s from %s", resp.Status, redactURL(rawURL))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, 0, 0, false, fmt.Errorf("api: unexpected status %s from %s", resp.Status, redactURL(rawURL))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, nil, 0, 0, false, fmt.Errorf("api: read body: %w", err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, nil, int64(len(body)), 0, false, fmt.Errorf("api: response from %s exceeds %d bytes", redactURL(rawURL), maxResponseBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, nil, int64(len(body)), 0, false, fmt.Errorf("api: decode JSON from %s: %w", redactURL(rawURL), err)
	}
	return root, resp.Header, int64(len(body)), 0, false, nil
}

// retryAfter parses an HTTP Retry-After header value: either delta-seconds (an
// integer) or an HTTP-date. It returns the delay and whether a value was present and
// parseable. A past date yields a zero (non-negative) delay.
func retryAfter(h http.Header) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// sleepCtx sleeps for d unless ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// nextLink extracts the rel="next" target from an RFC 5988 Link header, or "".
func nextLink(header string) string {
	if header == "" {
		return ""
	}
	for _, part := range strings.Split(header, ",") {
		seg := strings.TrimSpace(part)
		lt := strings.Index(seg, "<")
		gt := strings.Index(seg, ">")
		if lt != 0 || gt < 0 {
			continue
		}
		target := seg[lt+1 : gt]
		params := seg[gt+1:]
		if linkRelIsNext(params) {
			return target
		}
	}
	return ""
}

// linkRelIsNext reports whether the link-param string declares rel="next" (or rel=next).
func linkRelIsNext(params string) bool {
	for _, p := range strings.Split(params, ";") {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "rel") {
			continue
		}
		eq := strings.Index(p, "=")
		if eq < 0 {
			continue
		}
		val := strings.TrimSpace(p[eq+1:])
		val = strings.Trim(val, `"'`)
		for _, rel := range strings.Fields(val) {
			if strings.EqualFold(rel, "next") {
				return true
			}
		}
	}
	return false
}

func resolveRef(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

// withQuery merges q into rawURL's existing query.
func withQuery(rawURL string, q url.Values) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	existing := u.Query()
	for k, vs := range q {
		for _, v := range vs {
			existing.Set(k, v)
		}
	}
	u.RawQuery = existing.Encode()
	return u.String()
}

func paramOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// redactURL strips the query string from a URL for error/log messages, so a secret
// accidentally placed in a query param (or a cursor value) is never surfaced.
func redactURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		u.RawQuery = ""
		return u.String()
	}
	return rawURL
}
