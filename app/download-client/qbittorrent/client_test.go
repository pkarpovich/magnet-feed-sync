package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/types"
)

type fakeQbit struct {
	server   *httptest.Server
	torrents []map[string]string

	// raw body served by torrents/info, used for the entries recorded from the real instance;
	// it wins over torrents when set
	torrentsBody string

	addedTorrentIds []string
	addPlainText    bool
	addFailureCount int

	addSavePath string
	addURL      string

	torrentsHashes string

	setLocationHashes   string
	setLocationLocation string

	addStatus         int
	setLocationStatus int
	torrentsStatus    int
}

func newFakeQbit(t *testing.T) *fakeQbit {
	t.Helper()

	f := &fakeQbit{
		addStatus:         http.StatusOK,
		setLocationStatus: http.StatusOK,
		torrentsStatus:    http.StatusOK,
		addedTorrentIds:   []string{"abc123"},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "test-session"})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.addSavePath = r.FormValue("savepath")
		f.addURL = r.FormValue("urls")

		if f.addStatus == http.StatusConflict {
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte("Conflict"))
			return
		}

		if f.addStatus != http.StatusOK {
			w.WriteHeader(f.addStatus)
			return
		}

		// what every qbittorrent below the WebAPI version that added `added_torrent_ids` answers
		if f.addPlainText {
			w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Ok."))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"added_torrent_ids": f.addedTorrentIds,
			"success_count":     len(f.addedTorrentIds),
			"failure_count":     f.addFailureCount,
			"pending_count":     0,
		})
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.torrentsHashes = r.FormValue("hashes")

		if f.torrentsStatus != http.StatusOK {
			w.WriteHeader(f.torrentsStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if f.torrentsBody != "" {
			_, _ = w.Write([]byte(f.torrentsBody))
			return
		}
		_ = json.NewEncoder(w).Encode(f.torrents)
	})
	mux.HandleFunc("/api/v2/torrents/setLocation", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.setLocationHashes = r.FormValue("hashes")
		f.setLocationLocation = r.FormValue("location")
		w.WriteHeader(f.setLocationStatus)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeQbit) client() *Client {
	return NewClient(config.QBittorrentConfig{
		URL:         f.server.URL,
		Username:    "admin",
		Password:    "adminpass",
		Destination: "/downloads/default",
	})
}

func TestCreateDownloadTask(t *testing.T) {
	tests := []struct {
		name          string
		source        string
		status        int
		addedIds      []string
		plainText     bool
		failureCount  int
		wantHash      string
		wantErr       bool
		wantDuplicate bool
	}{
		{
			name:     "hash from the add response",
			source:   "magnet:?xt=urn:btih:abc",
			status:   http.StatusOK,
			addedIds: []string{"474d1403945c0768506233481557516e7af8d136"},
			wantHash: "474d1403945c0768506233481557516e7af8d136",
		},
		{
			name:     "btih fallback when the response carries no ids",
			source:   "magnet:?xt=urn:btih:2566E2B012EA1EF9087465BC97A7AC4449F4F0DE&dn=Some.Name",
			status:   http.StatusOK,
			addedIds: []string{},
			wantHash: "2566e2b012ea1ef9087465bc97a7ac4449f4f0de",
		},
		{
			// the add succeeded, so it must not be reported as a failure; the empty hash is what
			// the notify path checks for
			name:     "no ids and no magnet succeeds without a hash",
			source:   "https://jackett.example/dl/tpb/torrent.torrent?apikey=secret",
			status:   http.StatusOK,
			addedIds: []string{},
			wantHash: "",
		},
		{
			name:      "plain text ok is a success for a torrent url",
			source:    "https://jackett.example/dl/tpb/torrent.torrent?apikey=secret",
			status:    http.StatusOK,
			plainText: true,
			wantHash:  "",
		},
		{
			name:      "plain text ok still resolves a magnet from its btih",
			source:    "magnet:?xt=urn:btih:2566E2B012EA1EF9087465BC97A7AC4449F4F0DE&dn=Some.Name",
			status:    http.StatusOK,
			plainText: true,
			wantHash:  "2566e2b012ea1ef9087465bc97a7ac4449f4f0de",
		},
		{
			// a base32 infohash never matches what torrents/info reports, so it must not be
			// handed back as one: the sweep would read the miss as a deleted torrent
			name:      "base32 magnet yields no hash",
			source:    "magnet:?xt=urn:btih:EWLPFAJOUHX7CB2GK6ZF5J6EIRHU6EG6",
			status:    http.StatusOK,
			plainText: true,
			wantHash:  "",
		},
		{
			// 200 with a failure count is a refusal: the btih fallback would otherwise report a
			// hash for a torrent qbittorrent never took, and the sweep would call it deleted
			name:         "a refused source is an error even on 200",
			source:       "magnet:?xt=urn:btih:2566E2B012EA1EF9087465BC97A7AC4449F4F0DE&dn=Some.Name",
			status:       http.StatusOK,
			addedIds:     []string{},
			failureCount: 1,
			wantErr:      true,
		},
		{
			name:          "conflict reports an already present torrent",
			source:        "magnet:?xt=urn:btih:abc",
			status:        http.StatusConflict,
			wantErr:       true,
			wantDuplicate: true,
		},
		{
			name:    "unsupported media type stays an ordinary failure",
			source:  "https://jackett.example/dl/tpb/torrent.torrent?apikey=secret",
			status:  http.StatusUnsupportedMediaType,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeQbit(t)
			fake.addStatus = tt.status
			fake.addPlainText = tt.plainText
			fake.addFailureCount = tt.failureCount
			if tt.addedIds != nil {
				fake.addedTorrentIds = tt.addedIds
			}

			hash, err := fake.client().CreateDownloadTask(tt.source, "/downloads/movies")

			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, hash)
				assert.Equal(t, tt.wantDuplicate, errors.Is(err, types.ErrTorrentAlreadyExists))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantHash, hash)
			assert.Equal(t, "/downloads/movies", fake.addSavePath)
			assert.Equal(t, tt.source, fake.addURL)
		})
	}
}

// the client library embeds the source in every add error, and the agent posts jackett `/dl/`
// links whose query carries JACKETT_API_KEY: that error is logged, so it must not carry the key
func TestCreateDownloadTaskErrorHidesTheAPIKey(t *testing.T) {
	const source = "https://jackett.example/dl/tpb/torrent.torrent?jackett_apikey=s3cret-key&path=abc"

	tests := []struct {
		name   string
		status int
	}{
		{name: "unsupported media type", status: http.StatusUnsupportedMediaType},
		{name: "unexpected status", status: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeQbit(t)
			fake.addStatus = tt.status

			_, err := fake.client().CreateDownloadTask(source, "/downloads/movies")

			require.Error(t, err)
			assert.NotContains(t, err.Error(), "s3cret-key")
			assert.Contains(t, err.Error(), "jackett_apikey=redacted")
		})
	}
}

func TestCreateDownloadTaskConflictSurvivesRedaction(t *testing.T) {
	fake := newFakeQbit(t)
	fake.addStatus = http.StatusConflict

	_, err := fake.client().CreateDownloadTask(
		"https://jackett.example/dl/tpb/torrent.torrent?jackett_apikey=s3cret-key", "/downloads/movies")

	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrTorrentAlreadyExists))
	assert.NotContains(t, err.Error(), "s3cret-key")
}

const completedTorrentBody = `[{"hash":"474d1403945c0768506233481557516e7af8d136","name":"sample.bin","state":"stalledUP",
  "progress":1,"completion_on":1786626099,"amount_left":0,
  "content_path":"/downloads/probe/sample.bin","save_path":"/downloads/probe",
  "size":4194304,"total_size":4194304,"added_on":1786626098,"eta":8640000}]`

const unfinishedTorrentBody = `[{"hash":"9ecd4676fd0f0474151a4b74a5958f42639cebdf","name":"ubuntu-24.04.1-desktop-amd64.iso",
  "state":"downloading","progress":0,"completion_on":-1,"amount_left":5173995520,
  "content_path":"/downloads/probe-magnet/ubuntu-24.04.1-desktop-amd64.iso",
  "save_path":"/downloads/probe-magnet","size":5173995520,"total_size":5173995520,
  "added_on":1786626090,"eta":8640000}]`

func TestTorrentStates(t *testing.T) {
	tests := []struct {
		name string
		body string
		want types.TorrentState
	}{
		{
			name: "completed entry",
			body: completedTorrentBody,
			want: types.TorrentState{
				Hash:         "474d1403945c0768506233481557516e7af8d136",
				Name:         "sample.bin",
				State:        "stalledUP",
				ContentPath:  "/downloads/probe/sample.bin",
				Progress:     1,
				CompletionOn: 1786626099,
				Size:         4194304,
			},
		},
		{
			name: "unfinished entry",
			body: unfinishedTorrentBody,
			want: types.TorrentState{
				Hash:         "9ecd4676fd0f0474151a4b74a5958f42639cebdf",
				Name:         "ubuntu-24.04.1-desktop-amd64.iso",
				State:        "downloading",
				ContentPath:  "/downloads/probe-magnet/ubuntu-24.04.1-desktop-amd64.iso",
				Progress:     0,
				CompletionOn: -1,
				Size:         5173995520,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeQbit(t)
			fake.torrentsBody = tt.body

			states, err := fake.client().TorrentStates(context.Background(), []string{tt.want.Hash})

			require.NoError(t, err)
			assert.Equal(t, tt.want.Hash, fake.torrentsHashes)
			require.Len(t, states, 1)
			assert.Equal(t, tt.want, states[tt.want.Hash])
		})
	}
}

func TestTorrentStatesOmitsUnknownHash(t *testing.T) {
	fake := newFakeQbit(t)
	fake.torrentsBody = completedTorrentBody

	hashes := []string{"474d1403945c0768506233481557516e7af8d136", "9ecd4676fd0f0474151a4b74a5958f42639cebdf"}
	states, err := fake.client().TorrentStates(context.Background(), hashes)

	require.NoError(t, err)
	assert.Equal(t, strings.Join(hashes, "|"), fake.torrentsHashes)
	assert.Len(t, states, 1)
	_, found := states["9ecd4676fd0f0474151a4b74a5958f42639cebdf"]
	assert.False(t, found, "a hash qbittorrent does not know must be absent, never zero-valued")
}

func TestTorrentStatesError(t *testing.T) {
	fake := newFakeQbit(t)
	fake.torrentsStatus = http.StatusInternalServerError

	states, err := fake.client().TorrentStates(context.Background(), []string{"474d1403945c0768506233481557516e7af8d136"})

	require.Error(t, err)
	assert.Nil(t, states)
}

func TestGetHashByMagnet(t *testing.T) {
	tests := []struct {
		name     string
		torrents []map[string]string
		magnet   string
		wantHash string
		wantErr  bool
	}{
		{
			name: "matches by btih hash ignoring dn and case",
			torrents: []map[string]string{
				{"hash": "HASH1", "magnet_uri": "magnet:?xt=urn:btih:2566E2B012EA1EF9087465BC97A7AC4449F4F0DE&dn=Some.Name"},
			},
			magnet:   "magnet:?xt=urn:btih:2566e2b012ea1ef9087465bc97a7ac4449f4f0de",
			wantHash: "HASH1",
		},
		{
			name: "not found when no torrent matches",
			torrents: []map[string]string{
				{"hash": "HASH1", "magnet_uri": "magnet:?xt=urn:btih:deadbeef"},
			},
			magnet:  "magnet:?xt=urn:btih:2566e2b012ea1ef9087465bc97a7ac4449f4f0de",
			wantErr: true,
		},
		{
			name: "no false match when queried magnet has no btih hash",
			torrents: []map[string]string{
				{"hash": "HASH1", "magnet_uri": "magnet:?xt=urn:btmh:1220caf1e1c30e81cb361b9ee167c4aa64228a"},
			},
			magnet:  "magnet:?xt=urn:btmh:1220ffffffffffffffffffffffffffffffffffff",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeQbit(t)
			fake.torrents = tt.torrents

			hash, err := fake.client().GetHashByMagnet(tt.magnet)

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantHash, hash)
		})
	}
}

func TestGetHashByMagnet_GetTorrentsError(t *testing.T) {
	fake := newFakeQbit(t)
	fake.torrentsStatus = http.StatusInternalServerError

	_, err := fake.client().GetHashByMagnet("magnet:?xt=urn:btih:2566e2b012ea1ef9087465bc97a7ac4449f4f0de")

	require.Error(t, err)
}

func TestSetLocation(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "success", status: http.StatusOK},
		{name: "error on conflict", status: http.StatusConflict, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeQbit(t)
			fake.setLocationStatus = tt.status

			err := fake.client().SetLocation("HASH1", "/downloads/tv shows")

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "HASH1", fake.setLocationHashes)
			assert.Equal(t, "/downloads/tv shows", fake.setLocationLocation)
		})
	}
}

func TestGetLocations(t *testing.T) {
	locations := newFakeQbit(t).client().GetLocations()

	names := make(map[string]string, len(locations))
	for _, l := range locations {
		assert.True(t, strings.HasPrefix(l.ID, "/downloads/"), "location %q must live under /downloads/", l.ID)
		assert.NotEmpty(t, l.Name, "location %q needs a display name", l.ID)
		_, dup := names[l.ID]
		assert.False(t, dup, "duplicate location %q", l.ID)
		names[l.ID] = l.Name
	}

	assert.Equal(t, "Magazines", names["/downloads/magazines"])
	assert.Equal(t, "Cinema Prep", names["/downloads/cinema-prep"])
	assert.Equal(t, "Movies", names["/downloads/movies"])
}
