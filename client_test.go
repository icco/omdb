package omdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseScore(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"84", 84, true},
		{"0", 0, true},
		{"100", 100, true},
		{" 61 ", 61, true},
		{"N/A", 0, false},
		{"n/a", 0, false},
		{"", 0, false},
		{"101", 0, false},
		{"-1", 0, false},
		{"eight", 0, false},
	}
	for _, c := range cases {
		got, ok := parseScore(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("parseScore(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestParseYear(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"1999":      1999,
		"2010–2015": 2010, // en dash range
		"2010-2015": 2010,
		"2010–":     2010, // still running
		"N/A":       0,
		"":          0,
	}
	for in, want := range cases {
		if got := parseYear(in); got != want {
			t.Errorf("parseYear(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestMetascorePrefersTopLevelField(t *testing.T) {
	t.Parallel()
	r := &apiResponse{Metascore: "73"}
	r.Ratings = append(r.Ratings, struct {
		Source string `json:"Source"`
		Value  string `json:"Value"`
	}{Source: "Metacritic", Value: "40/100"})

	got := r.metascore()
	if got == nil || *got != 73 {
		t.Fatalf("metascore() = %v, want 73", got)
	}
}

func TestMetascoreFallsBackToRatings(t *testing.T) {
	t.Parallel()
	r := &apiResponse{Metascore: "N/A"}
	r.Ratings = append(r.Ratings, struct {
		Source string `json:"Source"`
		Value  string `json:"Value"`
	}{Source: "Internet Movie Database", Value: "8.7/10"})
	r.Ratings = append(r.Ratings, struct {
		Source string `json:"Source"`
		Value  string `json:"Value"`
	}{Source: "Metacritic", Value: "94/100"})

	got := r.metascore()
	if got == nil || *got != 94 {
		t.Fatalf("metascore() = %v, want 94", got)
	}
}

func TestMetascoreAbsentIsNil(t *testing.T) {
	t.Parallel()
	r := &apiResponse{Metascore: "N/A"}
	if got := r.metascore(); got != nil {
		t.Fatalf("metascore() = %v, want nil", *got)
	}
}

func TestGetByIMDbIDSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("i"); got != "tt0133093" {
			t.Errorf("imdb id = %q, want tt0133093", got)
		}
		if got := r.URL.Query().Get("apikey"); got != "secret" {
			t.Errorf("apikey = %q, want secret", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Title":"The Matrix","Year":"1999","Type":"movie","Metascore":"73","Response":"True"}`))
	}))
	defer srv.Close()

	c := NewClient("secret")
	c.BaseURL = srv.URL

	got, err := c.GetByIMDbID(context.Background(), "tt0133093")
	if err != nil {
		t.Fatalf("GetByIMDbID: %v", err)
	}
	if got.Title != "The Matrix" || got.Year != 1999 || got.Type != "movie" {
		t.Errorf("got %+v", got)
	}
	if got.Metascore == nil || *got.Metascore != 73 {
		t.Errorf("Metascore = %v, want 73", got.Metascore)
	}
}

// OMDb reports a missing title with HTTP 200 and Response:"False", which must
// not be retried or mistaken for a successful empty result.
func TestGetByIMDbIDNotFound(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"Response":"False","Error":"Incorrect IMDb ID."}`))
	}))
	defer srv.Close()

	c := NewClient("secret")
	c.BaseURL = srv.URL

	_, err := c.GetByIMDbID(context.Background(), "tt0000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on not-found)", calls)
	}
}

// A series with no Metacritic coverage must come back with a nil Metascore
// rather than a zero, so callers can tell "unscored" from "scored 0".
func TestGetByIMDbIDSeriesWithoutMetascore(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Title":"Some Show","Year":"2010–2015","Type":"series","Metascore":"N/A","Ratings":[],"Response":"True"}`))
	}))
	defer srv.Close()

	c := NewClient("secret")
	c.BaseURL = srv.URL

	got, err := c.GetByIMDbID(context.Background(), "tt1234567")
	if err != nil {
		t.Fatalf("GetByIMDbID: %v", err)
	}
	if got.Metascore != nil {
		t.Errorf("Metascore = %d, want nil", *got.Metascore)
	}
	if got.Year != 2010 {
		t.Errorf("Year = %d, want 2010", got.Year)
	}
}

func TestGetByIMDbIDEmptyID(t *testing.T) {
	t.Parallel()
	c := NewClient("secret")
	if _, err := c.GetByIMDbID(context.Background(), "  "); err == nil {
		t.Fatal("expected error for empty imdb id")
	}
}

// The api key must never reach an error string, since those are logged.
func TestErrorsDoNotLeakAPIKey(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"Response":"False","Error":"Invalid API key!"}`))
	}))
	defer srv.Close()

	c := NewClient("super-secret-key")
	c.BaseURL = srv.URL

	_, err := c.GetByIMDbID(context.Background(), "tt0133093")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret-key") {
		t.Errorf("error leaked api key: %v", err)
	}
}

// --- Coverage for the surface added when this package was made public. ---

// newFast returns a client pointed at srv with retries and pacing disabled, so
// tests do not sit through the real backoff.
func newFast(srv *httptest.Server, opts ...Option) *Client {
	c := NewClient("secret", append([]Option{WithAttempts(1)}, opts...)...)
	c.BaseURL = srv.URL
	return c
}

func serveJSON(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGetByIMDbIDParsesAllScores(t *testing.T) {
	t.Parallel()
	srv := serveJSON(t, `{"Title":"The Matrix","Year":"1999","Type":"movie",
		"Metascore":"73","imdbRating":"8.7","imdbVotes":"2,043,876",
		"Ratings":[{"Source":"Internet Movie Database","Value":"8.7/10"},
		           {"Source":"Rotten Tomatoes","Value":"83%"},
		           {"Source":"Metacritic","Value":"73/100"}],
		"Response":"True"}`)

	got, err := newFast(srv).GetByIMDbID(t.Context(), "tt0133093")
	if err != nil {
		t.Fatalf("GetByIMDbID: %v", err)
	}
	if got.IMDbRating == nil || *got.IMDbRating != 8.7 {
		t.Errorf("IMDbRating = %v, want 8.7", got.IMDbRating)
	}
	if got.IMDbVotes != 2043876 {
		t.Errorf("IMDbVotes = %d, want 2043876 (commas stripped)", got.IMDbVotes)
	}
	if got.RottenTomatoes == nil || *got.RottenTomatoes != 83 {
		t.Errorf("RottenTomatoes = %v, want 83", got.RottenTomatoes)
	}
	if got.Metascore == nil || *got.Metascore != 73 {
		t.Errorf("Metascore = %v, want 73", got.Metascore)
	}
}

// Every score is optional. A record with none must come back with nils rather
// than zeros, so callers can tell "unscored" from "scored zero".
func TestGetByIMDbIDAbsentScoresAreNil(t *testing.T) {
	t.Parallel()
	srv := serveJSON(t, `{"Title":"Obscure","Year":"1977","Type":"movie",
		"Metascore":"N/A","imdbRating":"N/A","imdbVotes":"N/A","Ratings":[],"Response":"True"}`)

	got, err := newFast(srv).GetByIMDbID(t.Context(), "tt0000001")
	if err != nil {
		t.Fatalf("GetByIMDbID: %v", err)
	}
	if got.Metascore != nil || got.IMDbRating != nil || got.RottenTomatoes != nil {
		t.Errorf("want all nil scores, got %+v", got)
	}
	if got.IMDbVotes != 0 {
		t.Errorf("IMDbVotes = %d, want 0", got.IMDbVotes)
	}
}

func TestGetByTitle(t *testing.T) {
	t.Parallel()
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"Title":"The Matrix","Year":"1999","Type":"movie","Response":"True"}`))
	}))
	defer srv.Close()

	c := NewClient("secret", WithAttempts(1))
	c.BaseURL = srv.URL
	got, err := c.GetByTitle(t.Context(), "The Matrix", 1999)
	if err != nil {
		t.Fatalf("GetByTitle: %v", err)
	}
	if got.Title != "The Matrix" {
		t.Errorf("Title = %q", got.Title)
	}
	if gotQuery.Get("t") != "The Matrix" {
		t.Errorf("t = %q", gotQuery.Get("t"))
	}
	if gotQuery.Get("y") != "1999" {
		t.Errorf("y = %q, want 1999", gotQuery.Get("y"))
	}
}

// Year 0 means "unconstrained" and must be omitted, not sent as y=0.
func TestGetByTitleOmitsZeroYear(t *testing.T) {
	t.Parallel()
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"Title":"X","Year":"1999","Response":"True"}`))
	}))
	defer srv.Close()

	c := NewClient("secret", WithAttempts(1))
	c.BaseURL = srv.URL
	if _, err := c.GetByTitle(t.Context(), "X", 0); err != nil {
		t.Fatalf("GetByTitle: %v", err)
	}
	if gotQuery.Has("y") {
		t.Errorf("y sent for an unconstrained year: %q", gotQuery.Get("y"))
	}
}

func TestGetByTitleRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := NewClient("secret").GetByTitle(t.Context(), "  ", 0); err == nil {
		t.Fatal("want an error for an empty title")
	}
}

func TestWithAttemptsRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"Title":"X","Year":"1999","Response":"True"}`))
	}))
	defer srv.Close()

	var retries []int
	c := NewClient("secret",
		WithAttempts(3),
		WithCircuitBreaker(10, time.Minute), // do not trip mid-test
		OnRetry(func(attempt int, _ error) { retries = append(retries, attempt) }),
	)
	c.BaseURL = srv.URL

	if _, err := c.GetByIMDbID(t.Context(), "tt1"); err != nil {
		t.Fatalf("GetByIMDbID: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if len(retries) != 2 {
		t.Errorf("OnRetry fired %d times, want 2", len(retries))
	}
}

func TestCircuitOpensAndShortCircuits(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient("secret", WithAttempts(1), WithCircuitBreaker(2, time.Minute))
	c.BaseURL = srv.URL

	for range 2 {
		if _, err := c.GetByIMDbID(t.Context(), "tt1"); err == nil {
			t.Fatal("want an error from a 500")
		}
	}
	before := calls
	_, err := c.GetByIMDbID(t.Context(), "tt1")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if calls != before {
		t.Errorf("made %d more requests behind an open breaker, want 0", calls-before)
	}
}

// A not-found is the service answering correctly, so it must not count toward
// opening the breaker.
func TestNotFoundDoesNotTripBreaker(t *testing.T) {
	t.Parallel()
	srv := serveJSON(t, `{"Response":"False","Error":"Incorrect IMDb ID."}`)

	c := NewClient("secret", WithAttempts(1), WithCircuitBreaker(2, time.Minute))
	c.BaseURL = srv.URL

	for range 5 {
		if _, err := c.GetByIMDbID(t.Context(), "tt0"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if _, err := c.GetByIMDbID(t.Context(), "tt0"); errors.Is(err, ErrCircuitOpen) {
		t.Error("breaker opened on not-found results")
	}
}

func TestWithUserAgentAndHTTPClient(t *testing.T) {
	t.Parallel()
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"Title":"X","Year":"1999","Response":"True"}`))
	}))
	defer srv.Close()

	used := false
	custom := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})}

	c := NewClient("secret", WithAttempts(1), WithUserAgent("myapp/1.0"), WithHTTPClient(custom))
	c.BaseURL = srv.URL
	if _, err := c.GetByIMDbID(t.Context(), "tt1"); err != nil {
		t.Fatalf("GetByIMDbID: %v", err)
	}
	if gotUA != "myapp/1.0" {
		t.Errorf("User-Agent = %q, want myapp/1.0", gotUA)
	}
	if !used {
		t.Error("the supplied http.Client was not used")
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRateLimiterPacesRequests(t *testing.T) {
	t.Parallel()
	srv := serveJSON(t, `{"Title":"X","Year":"1999","Response":"True"}`)

	c := NewClient("secret", WithAttempts(1), WithRateLimit(2, time.Hour))
	c.BaseURL = srv.URL

	for range 2 {
		if _, err := c.GetByIMDbID(t.Context(), "tt1"); err != nil {
			t.Fatalf("GetByIMDbID: %v", err)
		}
	}
	// The window is full for an hour, so the third call can only end in the
	// context expiring.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.GetByIMDbID(ctx, "tt1"); err == nil {
		t.Fatal("third call = nil, want a context error while the window is full")
	}
}

func TestNewClientDefaults(t *testing.T) {
	t.Parallel()
	c := NewClient("k")
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, DefaultBaseURL)
	}
	if c.userAgent != DefaultUserAgent {
		t.Errorf("userAgent = %q, want %q", c.userAgent, DefaultUserAgent)
	}
	if c.attempts != 3 {
		t.Errorf("attempts = %d, want 3", c.attempts)
	}
}

func TestParseVotes(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"2,043,876": 2043876,
		"1234":      1234,
		"N/A":       0,
		"":          0,
		"-5":        0,
		"abc":       0,
	}
	for in, want := range cases {
		if got := parseVotes(in); got != want {
			t.Errorf("parseVotes(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAPIErrorMessage(t *testing.T) {
	t.Parallel()
	e := &APIError{StatusCode: 401, Message: "nope", URL: "https://x/?i=tt1", Method: http.MethodGet}
	if !strings.Contains(e.Error(), "401") || !strings.Contains(e.Error(), "tt1") {
		t.Errorf("Error() = %q", e.Error())
	}
}
