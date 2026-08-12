package watcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"magnet-feed-sync/app/tracker/providers"
)

const challengePage = `<html><head><title>Just a moment...</title></head><body><script>window._cf_chl_opt={};</script></body></html>`

type fakePageSolver struct {
	mu        sync.Mutex
	calls     []string
	cookies   []*http.Cookie
	userAgent string
	err       error
}

func (s *fakePageSolver) Solve(ctx context.Context, pageURL string) (*providers.SolvedPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, pageURL)

	if s.err != nil {
		return nil, s.err
	}

	return &providers.SolvedPage{
		Body:      []byte("<html>solved</html>"),
		Cookies:   s.cookies,
		UserAgent: s.userAgent,
	}, nil
}

func (s *fakePageSolver) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.calls)
}

func newFakeSolver() *fakePageSolver {
	return &fakePageSolver{
		cookies:   []*http.Cookie{{Name: "cf_clearance", Value: "abc"}, {Name: "extto_sess", Value: "42"}},
		userAgent: "Mozilla/5.0 (X11; Linux x86_64) FlareSolverr",
	}
}

func browseFixture(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "extto_browse.html"))
	require.NoError(t, err)

	return string(body)
}

// exttoServer serves handler and returns a source pointed at it, wired to a fake solver.
func exttoServer(t *testing.T, handler http.HandlerFunc) (*ExttoSource, *fakePageSolver) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	solver := newFakeSolver()

	return NewExttoSource(ExttoOptions{BaseURL: srv.URL, Solver: solver}), solver
}

func TestExttoSearchParsesRows(t *testing.T) {
	fixture := browseFixture(t)
	var request *http.Request
	source, solver := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		request = r.Clone(r.Context())
		_, err := w.Write([]byte(fixture))
		require.NoError(t, err)
	})

	results, err := source.Search(context.Background(), "One Night Only")
	require.NoError(t, err)
	require.Len(t, results, 3)

	assert.Equal(t, 1, solver.callCount())
	assert.Equal(t, "cf_clearance=abc; extto_sess=42", request.Header.Get("Cookie"))
	assert.Equal(t, "Mozilla/5.0 (X11; Linux x86_64) FlareSolverr", request.Header.Get("User-Agent"))
	assert.Equal(t, exttoBrowsePath, request.URL.Path)
	assert.Equal(t, "One Night Only", request.URL.Query().Get("q"))
	assert.Equal(t, exttoSort, request.URL.Query().Get("sort"))
	assert.Equal(t, exttoOrder, request.URL.Query().Get("order"))
	assert.Empty(t, request.URL.Query().Get("cat"))
	assert.Empty(t, request.URL.Query().Get("with_adult"))

	first := results[0]
	assert.Equal(t, SourceExtto, first.Source)
	assert.Equal(t, "20151803", first.ExternalID)
	assert.Equal(t, "Dune.Prophecy.S01.2160p.UHD...-MTeam", first.Title)
	assert.Equal(t, source.baseURL+"/dune-prophecy-s01-2160p-uhd-eur-blu-ray-hevc-hdr10-truehd-7-1-mteam-20151803/", first.PageURL)
	assert.Equal(t, "One Night Only", first.Query)
	assert.Equal(t, 9, first.Seeders)
	assert.Equal(t, time.Date(2025, time.May, 9, 0, 0, 0, 0, time.UTC), first.PublishedAt)
	assert.Empty(t, first.DownloadURL)

	second := results[1]
	assert.Equal(t, "20134504", second.ExternalID)
	assert.Equal(t, "One.Night.Only.2026.1080p.WEB-DL.DDP5.1.H264-GROUP", second.Title)
	assert.Equal(t, 1204, second.Seeders)

	// the third row's Age cell carries no title attribute, so the date is unknown rather
	// than parsed out of "7 years ago"
	assert.True(t, results[2].PublishedAt.IsZero())
	assert.Equal(t, 2, results[2].Seeders)
}

func TestExttoSearchRefreshesCookieOnceOnChallenge(t *testing.T) {
	fixture := browseFixture(t)
	var mu sync.Mutex
	var calls int
	source, solver := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()

		if first {
			w.WriteHeader(http.StatusForbidden)
			_, err := w.Write([]byte(challengePage))
			require.NoError(t, err)
			return
		}
		_, err := w.Write([]byte(fixture))
		require.NoError(t, err)
	})

	results, err := source.Search(context.Background(), "One Night Only")
	require.NoError(t, err)
	assert.Len(t, results, 3)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, calls)
	// one refresh before the first request and one after the challenge
	assert.Equal(t, 2, solver.callCount())
}

func TestExttoSearchFailsOnSecondChallenge(t *testing.T) {
	source, solver := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(challengePage))
		require.NoError(t, err)
	})

	_, err := source.Search(context.Background(), "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "challenge survived a cookie refresh")
	assert.Equal(t, 2, solver.callCount())
}

func TestExttoSearchWithoutRows(t *testing.T) {
	source, _ := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`<html><body><table><tbody><tr><td>No results found</td></tr></tbody></table></body></html>`))
		require.NoError(t, err)
	})

	results, err := source.Search(context.Background(), "nothing at all")
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestExttoSearchNonSuccessStatus(t *testing.T) {
	source, _ := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, err := w.Write([]byte("upstream is down"))
		require.NoError(t, err)
	})

	_, err := source.Search(context.Background(), "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected status 502")
}

func TestExttoSolverUnconfigured(t *testing.T) {
	source := NewExttoSource(ExttoOptions{})

	_, err := source.Search(context.Background(), "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flaresolverr is not configured")

	_, err = source.Magnet(context.Background(), "20151803", "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flaresolverr is not configured")
}

func TestExttoSolverFailureSurfaces(t *testing.T) {
	source, solver := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the site must not be called without a cookie")
	})
	solver.err = errors.New("flaresolverr is unreachable")

	_, err := source.Search(context.Background(), "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh extto cookie: flaresolverr is unreachable")
}

func TestExttoSolverWithoutCookies(t *testing.T) {
	source, solver := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the site must not be called without a cookie")
	})
	solver.cookies = nil

	_, err := source.Search(context.Background(), "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "solver returned no cookies")
}

// TestExttoSignatureGoldenVector pins the signature against the vector captured from the
// live site. The expectation is a literal on purpose — recomputing it here would assert
// only that the code agrees with itself.
func TestExttoSignatureGoldenVector(t *testing.T) {
	source := NewExttoSource(ExttoOptions{})

	assert.Equal(t,
		"e74f1d43b90e6c4238ae05fb87d9b67b5029f46a384a9336a3f3398069429f84",
		source.sign("20151803", "1760000000", "a33f33f6d813fecdb8e79f5bd3587b6d"),
	)
}

func TestExttoMagnetSignsWithSearchPageToken(t *testing.T) {
	fixture := browseFixture(t)
	var mu sync.Mutex
	var form url.Values
	var header http.Header
	source, _ := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == exttoMagnetPath {
			require.NoError(t, r.ParseForm())
			mu.Lock()
			form = r.PostForm
			header = r.Header.Clone()
			mu.Unlock()
			_, err := w.Write([]byte(`{"success":true,"url":"magnet:?xt=urn:btih:deadbeef"}`))
			require.NoError(t, err)
			return
		}
		_, err := w.Write([]byte(fixture))
		require.NoError(t, err)
	})

	magnet, err := source.Magnet(context.Background(), "20151803", "One Night Only")
	require.NoError(t, err)
	assert.Equal(t, "magnet:?xt=urn:btih:deadbeef", magnet)

	mu.Lock()
	defer mu.Unlock()
	for _, field := range []string{"torrent_id", "hash", "name", "timestamp", "hmac", "sessid"} {
		assert.Contains(t, form, field, "form must carry %s", field)
	}
	assert.Equal(t, "20151803", form.Get("torrent_id"))
	assert.Empty(t, form.Get("hash"))
	assert.Empty(t, form.Get("name"))
	// the csrf token from the meta tag, not the search-page token
	assert.Equal(t, "7c4d0e0e6d1f4b2a9f3c8d5e1a2b3c4d", form.Get("sessid"))
	assert.Equal(t,
		source.sign("20151803", form.Get("timestamp"), "a33f33f6d813fecdb8e79f5bd3587b6d"),
		form.Get("hmac"),
	)
	assert.Equal(t, "XMLHttpRequest", header.Get("X-Requested-With"))
	assert.Equal(t, "application/x-www-form-urlencoded; charset=UTF-8", header.Get("Content-Type"))
	assert.Equal(t, source.baseURL, header.Get("Origin"))
}

func TestExttoMagnetReusesWarmTokens(t *testing.T) {
	fixture := browseFixture(t)
	var mu sync.Mutex
	var searches int
	source, _ := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == exttoMagnetPath {
			_, err := w.Write([]byte(`{"success":true,"url":"magnet:?xt=urn:btih:deadbeef"}`))
			require.NoError(t, err)
			return
		}
		mu.Lock()
		searches++
		mu.Unlock()
		_, err := w.Write([]byte(fixture))
		require.NoError(t, err)
	})

	_, err := source.Search(context.Background(), "One Night Only")
	require.NoError(t, err)

	_, err = source.Magnet(context.Background(), "20151803", "One Night Only")
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, searches, "the tokens of the preceding search must be reused")
}

func TestExttoMagnetRefused(t *testing.T) {
	fixture := browseFixture(t)
	source, _ := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == exttoMagnetPath {
			_, err := w.Write([]byte(`{"success":false}`))
			require.NoError(t, err)
			return
		}
		_, err := w.Write([]byte(fixture))
		require.NoError(t, err)
	})

	_, err := source.Magnet(context.Background(), "20151803", "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused for torrent 20151803")
}

func TestExttoMagnetNotRetriedOnChallenge(t *testing.T) {
	fixture := browseFixture(t)
	var mu sync.Mutex
	var posts int
	source, _ := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == exttoMagnetPath {
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_, err := w.Write([]byte(challengePage))
			require.NoError(t, err)
			return
		}
		_, err := w.Write([]byte(fixture))
		require.NoError(t, err)
	})

	_, err := source.Magnet(context.Background(), "20151803", "One Night Only")
	require.Error(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, posts, "a signed post is never replayed")
}

// a page whose token markup ext.to changed under us must be searched once, not once per
// row: a search is a full challenge-fenced round trip holding the shared solver, and the
// http search endpoint resolves a magnet for every ext.to row it answers with
func TestExttoMagnetDoesNotReplaySearchPerRowWithoutTokens(t *testing.T) {
	var mu sync.Mutex
	var searches int
	source, _ := exttoServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		searches++
		mu.Unlock()
		// no searchPageToken and no csrf meta tag
		_, err := w.Write([]byte(`<html><body><table><tbody></tbody></table></body></html>`))
		require.NoError(t, err)
	})

	for range 3 {
		_, err := source.Magnet(context.Background(), "20151803", "One Night Only")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no page tokens")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, searches, "the token-less page must be searched once, not once per row")
}

// a typed nil in the http layer's magnetResolver field passes its `!= nil` check, so the
// source itself has to survive being called on a nil receiver
func TestExttoNilSourceReportsInsteadOfPanicking(t *testing.T) {
	var source *ExttoSource

	_, err := source.Magnet(context.Background(), "20151803", "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flaresolverr is not configured")

	_, err = source.Search(context.Background(), "One Night Only")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flaresolverr is not configured")
}
