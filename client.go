// Package omdb is a Go client for the [OMDb API].
//
// It is deliberately small: a lookup by IMDb ID or title, returning the fields
// most callers want, wrapped in the resilience a quota-limited free API needs —
// sliding-window rate limiting, a circuit breaker, bounded retries, and a URL
// that never carries the api key into an error string or a log line.
//
// [OMDb API]: https://www.omdbapi.com/
package omdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the public OMDb endpoint.
const DefaultBaseURL = "https://www.omdbapi.com/"

// DefaultUserAgent identifies this client to OMDb.
const DefaultUserAgent = "github.com/icco/omdb"

// ErrCircuitOpen lets callers short-circuit retry and log loops when OMDb is
// known-down. Match it with errors.Is.
var ErrCircuitOpen = errors.New("omdb: circuit open")

// ErrNotFound is returned when OMDb has no record for the request. OMDb signals
// this with HTTP 200 and Response:"False", which is easy to mistake for a
// successful empty result.
var ErrNotFound = errors.New("omdb: title not found")

// Client is an OMDb API client. The api key is attached to outbound requests
// inside do and is never copied into errors or logs.
type Client struct {
	apiKey string
	// BaseURL is the OMDb endpoint; exported so tests can point at a stub.
	BaseURL        string
	userAgent      string
	httpClient     *http.Client
	rateLimiter    *rateLimiter
	circuitBreaker *circuitBreaker
	attempts       int
	onRetry        func(attempt int, err error)
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client. Use it to supply your own timeout,
// transport, or instrumentation; this package adds none of its own.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithRateLimit sets the sliding-window rate limit. The default is 20 requests
// per 10 seconds, which is about pacing a batch rather than any documented cap:
// OMDb's free tier is a daily quota (1,000 requests), so bound your batch size
// as well.
func WithRateLimit(maxRequests int, window time.Duration) Option {
	return func(c *Client) { c.rateLimiter = newRateLimiter(maxRequests, window) }
}

// WithCircuitBreaker sets how many consecutive failures open the breaker and
// how long it stays open. The default is 5 failures and 60 seconds.
func WithCircuitBreaker(maxFailures int, timeout time.Duration) Option {
	return func(c *Client) { c.circuitBreaker = newCircuitBreaker(maxFailures, timeout) }
}

// WithAttempts sets the total number of tries per lookup, including the first.
// The default is 3. Values below 1 are treated as 1.
func WithAttempts(n int) Option {
	return func(c *Client) { c.attempts = n }
}

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// OnRetry registers a callback invoked after each failed attempt that will be
// retried. It exists so callers can log without this package taking a logging
// dependency.
func OnRetry(f func(attempt int, err error)) Option {
	return func(c *Client) { c.onRetry = f }
}

// APIError is a non-200 response from OMDb. URL never contains the api key.
type APIError struct {
	StatusCode int
	Message    string
	URL        string
	Method     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("omdb: HTTP %d %s for %s %s", e.StatusCode, e.Message, e.Method, e.URL)
}

// Title is an OMDb record. Score fields are pointers because OMDb reports a
// missing score as the string "N/A", and a nil pointer keeps "unscored"
// distinguishable from "scored zero".
type Title struct {
	Title string
	Year  int
	// Type is OMDb's media type: "movie", "series", or "episode".
	Type string
	// Metascore is the Metacritic critic score (0-100), or nil when Metacritic
	// has no score. Metacritic scores TV by season rather than by series, so
	// this is frequently absent for shows.
	Metascore *int
	// IMDbRating is the IMDb user score on its native 0-10 scale, or nil.
	IMDbRating *float64
	// IMDbVotes is how many users contributed to IMDbRating, or 0 when absent.
	IMDbVotes int
	// RottenTomatoes is the Tomatometer percentage (0-100), or nil.
	RottenTomatoes *int
}

// NewClient returns an OMDb client. Get a free api key at
// https://www.omdbapi.com/apikey.aspx.
func NewClient(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:    apiKey,
		BaseURL:   DefaultBaseURL,
		userAgent: DefaultUserAgent,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
			},
		},
		rateLimiter:    newRateLimiter(20, 10*time.Second),
		circuitBreaker: newCircuitBreaker(5, 60*time.Second),
		attempts:       3,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// do builds a request from safeURL (which carries no api key) and attaches the
// key just before sending, so the key never reaches an error string or a log.
func (c *Client) do(ctx context.Context, safeURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, safeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("omdb: build request: %w", err)
	}
	q := req.URL.Query()
	q.Set("apikey", c.apiKey)
	req.URL.RawQuery = q.Encode()

	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req) //nolint:gosec // G704: BaseURL is the caller's to configure
	if err != nil {
		// Discard err.Error(): Go's net/http embeds the request URL, which by
		// this point carries the api key, in the error message.
		return nil, errors.New("omdb: transport error")
	}
	return resp, nil
}

// apiResponse is the raw OMDb payload. Numeric fields arrive as strings and may
// be the literal "N/A", so everything is decoded as a string and parsed.
type apiResponse struct {
	Title      string `json:"Title"`
	Year       string `json:"Year"`
	Type       string `json:"Type"`
	Metascore  string `json:"Metascore"`
	IMDbRating string `json:"imdbRating"`
	IMDbVotes  string `json:"imdbVotes"`
	Ratings    []struct {
		Source string `json:"Source"`
		Value  string `json:"Value"`
	} `json:"Ratings"`
	Response string `json:"Response"`
	Error    string `json:"Error"`
}

// GetByIMDbID looks a title up by IMDb ID (e.g. "tt0133093"). It returns
// ErrNotFound when OMDb has no such record and ErrCircuitOpen when the breaker
// has tripped.
func (c *Client) GetByIMDbID(ctx context.Context, imdbID string) (*Title, error) {
	if strings.TrimSpace(imdbID) == "" {
		return nil, errors.New("omdb: empty imdb id")
	}
	return c.fetch(ctx, url.Values{"i": {imdbID}})
}

// GetByTitle looks a title up by name. Pass year 0 to leave it unconstrained.
// Title search is fuzzy and OMDb returns a single match with no way to see the
// alternatives, so prefer GetByIMDbID whenever you have an id.
func (c *Client) GetByTitle(ctx context.Context, title string, year int) (*Title, error) {
	if strings.TrimSpace(title) == "" {
		return nil, errors.New("omdb: empty title")
	}
	q := url.Values{"t": {title}}
	if year > 0 {
		q.Set("y", strconv.Itoa(year))
	}
	return c.fetch(ctx, q)
}

// fetch runs one lookup with retries.
func (c *Client) fetch(ctx context.Context, q url.Values) (*Title, error) {
	// safeURL never includes the api key, so it is safe to embed in errors and logs.
	safeURL := strings.TrimSuffix(c.BaseURL, "?") + "?" + q.Encode()

	attempts := c.attempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		result, err := c.attempt(ctx, safeURL)
		if err == nil {
			return result, nil
		}
		lastErr = err

		// A missing title and an open breaker both fail identically on retry.
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCircuitOpen) {
			return nil, err
		}
		// Callers bound this work with a deadline; backing off past it would
		// overrun that budget to reach a request that cannot succeed anyway.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt == attempts {
			break
		}

		if c.onRetry != nil {
			c.onRetry(attempt, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return nil, lastErr
}

// attempt performs a single guarded request.
func (c *Client) attempt(ctx context.Context, safeURL string) (*Title, error) {
	if !c.circuitBreaker.canExecute() {
		return nil, ErrCircuitOpen
	}
	if err := c.rateLimiter.wait(ctx); err != nil {
		return nil, fmt.Errorf("omdb: rate limit wait cancelled: %w", err)
	}

	resp, err := c.do(ctx, safeURL)
	if err != nil {
		c.circuitBreaker.recordFailure()
		return nil, &APIError{StatusCode: 0, Message: "transport error", URL: safeURL, Method: http.MethodGet}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 500 {
			c.circuitBreaker.recordFailure()
		}
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Message:    string(body),
			URL:        safeURL,
			Method:     http.MethodGet,
		}
	}

	var raw apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		c.circuitBreaker.recordFailure()
		return nil, fmt.Errorf("omdb: decode response: %w", err)
	}
	c.circuitBreaker.recordSuccess()

	// OMDb signals "no such title" with HTTP 200 and Response:"False".
	if !strings.EqualFold(raw.Response, "True") {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, raw.Error)
	}

	return &Title{
		Title:          raw.Title,
		Year:           parseYear(raw.Year),
		Type:           raw.Type,
		Metascore:      raw.metascore(),
		IMDbRating:     raw.imdbRating(),
		IMDbVotes:      parseVotes(raw.IMDbVotes),
		RottenTomatoes: raw.ratingPercent("Rotten Tomatoes"),
	}, nil
}

// metascore reads the Metacritic score, preferring the top-level Metascore field
// and falling back to the "84/100" form in Ratings[]. Returns nil when absent,
// "N/A", or out of the 0-100 range, so callers can tell missing from zero.
func (r *apiResponse) metascore() *int {
	if n, ok := parseScore(r.Metascore); ok {
		return &n
	}
	return r.ratingPercent("Metacritic")
}

// ratingPercent pulls a 0-100 score out of the named Ratings[] entry, accepting
// both the "84/100" and "87%" forms OMDb uses.
func (r *apiResponse) ratingPercent(source string) *int {
	for _, rating := range r.Ratings {
		if !strings.EqualFold(rating.Source, source) {
			continue
		}
		value, _, _ := strings.Cut(rating.Value, "/")
		value = strings.TrimSuffix(strings.TrimSpace(value), "%")
		if n, ok := parseScore(value); ok {
			return &n
		}
	}
	return nil
}

// imdbRating parses the 0-10 IMDb score, nil when absent or "N/A".
func (r *apiResponse) imdbRating() *float64 {
	s := strings.TrimSpace(r.IMDbRating)
	if s == "" || strings.EqualFold(s, "N/A") {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > 10 {
		return nil
	}
	return &f
}

// parseScore parses an OMDb score string, rejecting "N/A", blanks, and
// out-of-range values.
func parseScore(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "N/A") {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 100 {
		return 0, false
	}
	return n, true
}

// parseVotes parses imdbVotes, which arrives comma-grouped ("1,234,567").
func parseVotes(s string) int {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || strings.EqualFold(s, "N/A") {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseYear parses OMDb's year field, which can be "1999", "2010–2015", or
// "2010–" for a running series. Returns 0 when unparseable.
func parseYear(s string) int {
	s = strings.TrimSpace(s)
	// Series ranges use an en dash; cut on both forms.
	for _, sep := range []string{"–", "-"} {
		if before, _, found := strings.Cut(s, sep); found {
			s = before
			break
		}
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
