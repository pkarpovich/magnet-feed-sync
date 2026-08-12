package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSolver struct {
	mu       sync.Mutex
	requests []solverRequest
	html     string
}

func (s *fakeSolver) record(r solverRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r)
}

func (s *fakeSolver) commands(cmd string) []solverRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []solverRequest
	for _, r := range s.requests {
		if r.Cmd == cmd {
			out = append(out, r)
		}
	}

	return out
}

func (s *fakeSolver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req solverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.record(req)

	w.Header().Set("Content-Type", "application/json")
	switch req.Cmd {
	case "sessions.create":
		_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully.","session":"` + req.Session + `"}`))
	case "sessions.destroy":
		_, _ = w.Write([]byte(`{"status":"ok","message":"The session has been removed."}`))
	case "request.get":
		body, err := json.Marshal(solverResponse{
			Status:   "ok",
			Message:  "Challenge not detected!",
			Solution: &solverSolution{Status: http.StatusOK, Response: s.html},
		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","message":"Error: Request parameter 'cmd' is invalid.","version":"3.5.0"}`))
	}
}

func TestSolverFetchSuccess(t *testing.T) {
	solver := &fakeSolver{html: `<html><a class="magnet-link" href="magnet:?xt=urn:btih:abc"></a></html>`}
	var contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		solver.ServeHTTP(w, r)
	}))
	defer server.Close()

	fetcher := NewSolverFetcher(server.URL)
	body, err := fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.NoError(t, err)

	assert.Contains(t, string(body), "magnet:?xt=urn:btih:abc")
	assert.Equal(t, "application/json", contentType)

	gets := solver.commands("request.get")
	require.Len(t, gets, 1)
	assert.Equal(t, "https://rutracker.org/forum/viewtopic.php?t=1", gets[0].URL)
	assert.Equal(t, solverMaxTimeout, gets[0].MaxTimeout)

	creates := solver.commands("sessions.create")
	require.Len(t, creates, 1)
	assert.True(t, strings.HasPrefix(creates[0].Session, "magnet-feed-sync-"))
	assert.Equal(t, creates[0].Session, gets[0].Session)
}

func TestSolverSessionReusedAcrossThreeFetches(t *testing.T) {
	solver := &fakeSolver{html: "<html>page</html>"}
	server := httptest.NewServer(solver)
	defer server.Close()

	fetcher := NewSolverFetcher(server.URL)
	for i := 0; i < 3; i++ {
		body, err := fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
		require.NoError(t, err)
		assert.Contains(t, string(body), "page")
	}

	creates := solver.commands("sessions.create")
	require.Len(t, creates, 1)

	gets := solver.commands("request.get")
	require.Len(t, gets, 3)
	for _, g := range gets {
		assert.Equal(t, creates[0].Session, g.Session)
	}
}

func TestSolverCloseDoesNotWaitOutInFlightFetch(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	solver := &fakeSolver{html: "<html>page</html>"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req solverRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Cmd == "request.get" {
			close(started)
			<-release
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		solver.ServeHTTP(w, r)
	}))
	defer server.Close()

	fetcher := NewSolverFetcher(server.URL)
	fetchDone := make(chan struct{})
	go func() {
		defer close(fetchDone)
		_, _ = fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := fetcher.Close(ctx)
	require.Error(t, err, "Close gives up instead of blocking on the in-flight fetch")
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	close(release)
	<-fetchDone
}

// TestSolverSolveReturnsSessionState covers the seam the ext.to source needs: the body is
// not enough, because presenting the cookie without its User-Agent re-triggers the
// challenge. The cookie's numeric `expires` is part of the fixture on purpose — it is why
// the solution cannot decode straight into net/http.Cookie.
func TestSolverSolveReturnsSessionState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req solverRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		w.Header().Set("Content-Type", "application/json")
		if req.Cmd != cmdRequestGet {
			_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","message":"Challenge solved!","solution":{
			"status":200,"response":"<html>browse</html>","userAgent":"Mozilla/5.0 Firefox",
			"cookies":[{"name":"cf_clearance","value":"abc","domain":".extto.com","path":"/","expires":1760000000.5},
			           {"name":"extto_sess","value":"42","domain":".extto.com","path":"/"}]}}`))
	}))
	defer server.Close()

	page, err := NewSolverFetcher(server.URL).Solve(context.Background(), "https://search.extto.com/")
	require.NoError(t, err)

	assert.Equal(t, "<html>browse</html>", string(page.Body))
	assert.Equal(t, "Mozilla/5.0 Firefox", page.UserAgent)
	require.Len(t, page.Cookies, 2)
	assert.Equal(t, "cf_clearance", page.Cookies[0].Name)
	assert.Equal(t, "abc", page.Cookies[0].Value)
	assert.Equal(t, ".extto.com", page.Cookies[0].Domain)
	assert.Equal(t, "extto_sess", page.Cookies[1].Name)
}

// Solve deliberately does not judge the body the way Fetch does: ext.to's cookie refresh
// solves the front page and detects challenges itself, so moving the marker check into the
// shared request path would make every refresh fail
func TestSolverSolveReturnsAChallengeBodyWithoutError(t *testing.T) {
	body := `<html><head><title>` + challengeMarker + `</title></head></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req solverRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		w.Header().Set("Content-Type", "application/json")
		if req.Cmd != cmdRequestGet {
			_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
			return
		}
		payload, err := json.Marshal(map[string]any{
			"status":  "ok",
			"message": "Challenge solved!",
			"solution": map[string]any{
				"status":    200,
				"response":  body,
				"userAgent": "Mozilla/5.0 Firefox",
				"cookies":   []map[string]string{{"name": "cf_clearance", "value": "abc"}},
			},
		})
		require.NoError(t, err)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	solver := NewSolverFetcher(server.URL)

	page, err := solver.Solve(context.Background(), "https://search.extto.com/")
	require.NoError(t, err)
	assert.Equal(t, body, string(page.Body))

	// Fetch, on the same body, still refuses
	_, err = solver.Fetch(context.Background(), "https://search.extto.com/")
	require.Error(t, err)
	var provErr *ProviderError
	require.ErrorAs(t, err, &provErr)
	assert.Equal(t, KindBlocked, provErr.Kind)
}

func TestSolverSolveReportsRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req solverRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		w.Header().Set("Content-Type", "application/json")
		if req.Cmd != cmdRequestGet {
			_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","solution":{"status":403,"response":"<html>denied</html>"}}`))
	}))
	defer server.Close()

	_, err := NewSolverFetcher(server.URL).Solve(context.Background(), "https://search.extto.com/")
	require.Error(t, err)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, KindBlocked, providerErr.Kind)
}

func TestSolverFetchGivesUpOnCanceledContext(t *testing.T) {
	solver := &fakeSolver{html: "<html>page</html>"}
	server := httptest.NewServer(solver)
	defer server.Close()

	fetcher := NewSolverFetcher(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetcher.Fetch(ctx, "https://rutracker.org/forum/viewtopic.php?t=1")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)

	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, KindTransient, providerErr.Kind)
}

func TestSolverCloseDestroysSession(t *testing.T) {
	solver := &fakeSolver{html: "<html>page</html>"}
	server := httptest.NewServer(solver)
	defer server.Close()

	fetcher := NewSolverFetcher(server.URL)
	require.NoError(t, fetcher.Close(context.Background()))
	assert.Empty(t, solver.commands("sessions.destroy"))

	_, err := fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.NoError(t, err)
	require.NoError(t, fetcher.Close(context.Background()))

	destroys := solver.commands("sessions.destroy")
	require.Len(t, destroys, 1)
	assert.Equal(t, solver.commands("sessions.create")[0].Session, destroys[0].Session)
}

// only a page fetch the tracker's protection refused may trip the breaker; the solver's own
// failures must stay transient or a container hiccup costs 24h of rutracker syncing
func TestSolverClassifiesFailedCommands(t *testing.T) {
	failingRequestGet := func(payload string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req solverRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

			w.Header().Set("Content-Type", "application/json")
			if req.Cmd != cmdRequestGet {
				_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
				return
			}

			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(payload))
		}
	}

	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    ErrorKind
	}{
		{
			// the session is created fine: it is the page fetch the solver could not complete
			name:    "challenge_not_solved",
			handler: failingRequestGet(`{"status":"error","message":"Error: Error solving the challenge. Timeout after 60.0 seconds.","version":"3.5.0"}`),
			want:    KindBlocked,
		},
		{
			name:    "solver_browser_failure",
			handler: failingRequestGet(`{"status":"error","message":"Error: Unable to process browser request. net::ERR_NAME_NOT_RESOLVED","version":"3.5.0"}`),
			want:    KindTransient,
		},
		{
			// a response shaped unlike what we expect is a solver-version problem, not a refusal
			name: "ok_status_without_solution",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"ok","message":"Challenge not detected!"}`))
			},
			want: KindTransient,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			body, err := NewSolverFetcher(server.URL).Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
			require.Error(t, err)
			assert.Nil(t, body)

			var pe *ProviderError
			require.True(t, errors.As(err, &pe))
			assert.Equal(t, tt.want, pe.Kind)
		})
	}
}

func TestSolverClassifiesTrackerStatus(t *testing.T) {
	tests := []struct {
		name     string
		solution solverSolution
		want     ErrorKind
	}{
		{name: "blocked_403", solution: solverSolution{Status: http.StatusForbidden, Response: "<html>denied</html>"}, want: KindBlocked},
		{name: "blocked_429", solution: solverSolution{Status: http.StatusTooManyRequests, Response: "<html>slow down</html>"}, want: KindBlocked},
		{name: "permanent_404", solution: solverSolution{Status: http.StatusNotFound, Response: "<html>gone</html>"}, want: KindPermanent},
		{name: "transient_500", solution: solverSolution{Status: http.StatusInternalServerError, Response: "<html>oops</html>"}, want: KindTransient},
		{
			name:     "blocked_cf_body",
			solution: solverSolution{Status: http.StatusOK, Response: "<html><title>Just a moment...</title></html>"},
			want:     KindBlocked,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req solverRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

				w.Header().Set("Content-Type", "application/json")
				if req.Cmd != "request.get" {
					_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
					return
				}

				body, err := json.Marshal(solverResponse{Status: "ok", Message: "Challenge not detected!", Solution: &tt.solution})
				require.NoError(t, err)
				_, _ = w.Write(body)
			}))
			defer server.Close()

			body, err := NewSolverFetcher(server.URL).Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
			require.Error(t, err)
			assert.Nil(t, body)

			var pe *ProviderError
			require.True(t, errors.As(err, &pe))
			assert.Equal(t, tt.want, pe.Kind)
		})
	}
}

func TestSolverMissingSolutionStatusIsAccepted(t *testing.T) {
	solver := &fakeSolver{html: "<html>page</html>"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req solverRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		solver.record(req)

		w.Header().Set("Content-Type", "application/json")
		if req.Cmd != "request.get" {
			_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","message":"ok","solution":{"response":"<html>page</html>"}}`))
	}))
	defer server.Close()

	body, err := NewSolverFetcher(server.URL).Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.NoError(t, err)
	assert.Contains(t, string(body), "page")
}

// a solver we cannot reach or parse is local infrastructure, not a tracker refusal: it must
// stay transient so a restarting flaresolverr never trips the provider for up to 24h
func TestSolverInfraFailureIsTransient(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		server.Close()

		_, err := NewSolverFetcher(server.URL).Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
		require.Error(t, err)

		var pe *ProviderError
		require.True(t, errors.As(err, &pe))
		assert.Equal(t, KindTransient, pe.Kind)
	})

	t.Run("invalid_body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer server.Close()

		_, err := NewSolverFetcher(server.URL).Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
		require.Error(t, err)

		var pe *ProviderError
		require.True(t, errors.As(err, &pe))
		assert.Equal(t, KindTransient, pe.Kind)
	})
}

// flaresolverr drops every session when it restarts and then rejects the stale id forever,
// so a failed fetch has to release it instead of pinning the provider to a dead session
func TestSolverRecreatesSessionAfterFailedFetch(t *testing.T) {
	solver := &fakeSolver{html: "<html>page</html>"}
	sessionAlive := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req solverRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		solver.record(req)

		w.Header().Set("Content-Type", "application/json")
		if req.Cmd == "sessions.create" {
			sessionAlive = true
			_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
			return
		}

		if !sessionAlive {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"error","message":"Error: This session does not exist."}`))
			return
		}

		body, err := json.Marshal(solverResponse{
			Status:   "ok",
			Solution: &solverSolution{Status: http.StatusOK, Response: solver.html},
		})
		require.NoError(t, err)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	fetcher := NewSolverFetcher(server.URL)
	_, err := fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.NoError(t, err)

	// the solver restarted: the session it handed out is gone
	sessionAlive = false

	_, err = fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.Error(t, err)
	assert.Empty(t, fetcher.sessionID)

	body, err := fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.NoError(t, err)
	assert.Contains(t, string(body), "page")
	assert.Len(t, solver.commands("sessions.create"), 2)
}

// only a lost session invalidates the id: dropping it after any other failure would leave a
// live browser behind on the solver and make the next fetch pay a ~74s cold solve
func TestSolverKeepsSessionAfterUnrelatedFailure(t *testing.T) {
	solver := &fakeSolver{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req solverRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		solver.record(req)

		w.Header().Set("Content-Type", "application/json")
		if req.Cmd != cmdRequestGet {
			_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
			return
		}

		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","message":"Error: Error solving the challenge. Timeout after 60.0 seconds."}`))
	}))
	defer server.Close()

	fetcher := NewSolverFetcher(server.URL)
	_, err := fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.Error(t, err)
	assert.NotEmpty(t, fetcher.sessionID)

	_, err = fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=2")
	require.Error(t, err)
	assert.Len(t, solver.commands("sessions.create"), 1, "the live session must be reused, not replaced")
}

// a solver that cannot even hand out a session is local infrastructure: classifying it as
// blocked would trip the provider for up to 24h over a flaresolverr restart
func TestSolverSessionCommandFailureIsTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","message":"Error: Unable to start the browser."}`))
	}))
	defer server.Close()

	_, err := NewSolverFetcher(server.URL).Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.Error(t, err)

	var pe *ProviderError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, KindTransient, pe.Kind)
}

// a stale session id must not be reported as a tracker refusal either
func TestSolverLostSessionIsTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req solverRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		w.Header().Set("Content-Type", "application/json")
		if req.Cmd != cmdRequestGet {
			_, _ = w.Write([]byte(`{"status":"ok","message":"Session created successfully."}`))
			return
		}

		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","message":"Error: This session does not exist."}`))
	}))
	defer server.Close()

	fetcher := NewSolverFetcher(server.URL)
	_, err := fetcher.Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.Error(t, err)
	assert.Empty(t, fetcher.sessionID)

	var pe *ProviderError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, KindTransient, pe.Kind)
}

func TestBlockedFetcherIsAlwaysBlocked(t *testing.T) {
	body, err := NewBlockedFetcher().Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.Error(t, err)
	assert.Nil(t, body)

	var pe *ProviderError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, KindBlocked, pe.Kind)
	assert.EqualError(t, err, "Blocked: flaresolverr not configured")
}
