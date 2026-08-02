# omdb

[![Go Reference](https://pkg.go.dev/badge/github.com/icco/omdb.svg)](https://pkg.go.dev/github.com/icco/omdb)
[![Test Go](https://github.com/icco/omdb/actions/workflows/test.yml/badge.svg)](https://github.com/icco/omdb/actions/workflows/test.yml)

A Go client for the [OMDb API](https://www.omdbapi.com/), with the resilience a quota-limited free API actually needs.

OMDb is the practical way to get a **Metacritic Metascore** in code — Metacritic has no public API of its own, and OMDb exposes the Metascore keyed by IMDb ID. It also carries the IMDb rating and the Tomatometer.

Get a free key at [omdbapi.com/apikey.aspx](https://www.omdbapi.com/apikey.aspx). The free tier allows 1,000 requests per day.

```
go get github.com/icco/omdb
```

## Usage

```go
c := omdb.NewClient(os.Getenv("OMDB_API_KEY"))

t, err := c.GetByIMDbID(ctx, "tt0133093")
if errors.Is(err, omdb.ErrNotFound) {
  // OMDb has no record for that id.
} else if errors.Is(err, omdb.ErrCircuitOpen) {
  // OMDb is known-down; skip the rest of this batch.
} else if err != nil {
  return err
}

fmt.Println(t.Title, t.Year) // The Matrix 1999
if t.Metascore != nil {
  fmt.Printf("Metacritic: %d\n", *t.Metascore)
}
if t.IMDbRating != nil {
  fmt.Printf("IMDb: %.1f (%d votes)\n", *t.IMDbRating, t.IMDbVotes)
}
```

Lookup by name is also available, but it is fuzzy — OMDb picks one match and gives you no way to see the alternatives. Prefer the id when you have one.

```go
t, err := c.GetByTitle(ctx, "The Matrix", 1999) // pass 0 to leave the year open
```

### Options

```go
c := omdb.NewClient(key,
  omdb.WithRateLimit(20, 10*time.Second),        // default
  omdb.WithCircuitBreaker(5, time.Minute),       // default
  omdb.WithAttempts(3),                          // default
  omdb.WithHTTPClient(myInstrumentedClient),
  omdb.WithUserAgent("myapp/1.0"),
  omdb.OnRetry(func(attempt int, err error) {
    slog.Warn("retrying omdb", "attempt", attempt, "err", err)
  }),
)
```

## Notes

- **Every score is a pointer, and that is deliberate.** OMDb reports a missing score as the string `"N/A"`. Decoding that to `0` makes an unrated title look like a panned one. `nil` keeps "unscored" distinguishable from "scored zero".
- **Metacritic has no useful TV coverage through OMDb.** Metacritic scores television by season, not by series, so `Metascore` is nil for most shows. Measured against a real key: 0 of 12 series returned a Metascore, by title or by IMDb id, and the season and episode endpoints carry no score field at all. Don't spend quota looking.
- **Not-found is HTTP 200.** OMDb signals a missing record with `Response: "False"` and a 200 status, which reads as a successful empty result. This client returns `ErrNotFound` instead, and does not retry it or count it against the circuit breaker — the service answered correctly.
- **The api key never reaches an error string.** Errors are built from a URL that has no key in it, and the key is attached inside the request just before sending. Transport errors are replaced wholesale, because Go's `net/http` embeds the full request URL in them.
- **`Year` handles series ranges.** `"2010–2015"` and `"2010–"` both parse to `2010`. OMDb uses an en dash; both dash forms are accepted.
- **`IMDbVotes` arrives comma-grouped** (`"2,043,876"`) and is parsed to an int.
- **The rate limit is pacing, not a documented cap.** OMDb's free tier is a daily quota, so bound your batch size as well as your rate.
- No third-party dependencies. The rate limiter and circuit breaker are a self-contained copy of [`icco/gutil/httpx`](https://github.com/icco/gutil/tree/main/httpx), duplicated rather than imported so adopting this client does not drag in a personal utility library.

## License

MIT
