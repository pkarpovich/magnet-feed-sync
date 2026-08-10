package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

func TestSolverErrorIsBlocked(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "http_500_status_error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"status":"error","message":"Error: Error solving the challenge. Timeout after 60.0 seconds.","version":"3.5.0"}`))
			},
		},
		{
			name: "ok_status_without_solution",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"status":"ok","message":"Challenge not detected!"}`))
			},
		},
		{
			name: "invalid_body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("not json"))
			},
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
			assert.Equal(t, KindBlocked, pe.Kind)
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

func TestSolverUnreachableIsBlocked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()

	_, err := NewSolverFetcher(server.URL).Fetch(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1")
	require.Error(t, err)

	var pe *ProviderError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, KindBlocked, pe.Kind)
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
