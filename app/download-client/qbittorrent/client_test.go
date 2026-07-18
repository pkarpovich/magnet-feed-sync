package qbittorrent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"magnet-feed-sync/app/config"
)

type fakeQbit struct {
	server   *httptest.Server
	torrents []map[string]string

	addSavePath string
	addURL      string

	setLocationHashes   string
	setLocationLocation string

	addStatus         int
	setLocationStatus int
}

func newFakeQbit(t *testing.T) *fakeQbit {
	t.Helper()

	f := &fakeQbit{addStatus: http.StatusOK, setLocationStatus: http.StatusOK}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "test-session"})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.addSavePath = r.FormValue("savepath")
		f.addURL = r.FormValue("urls")

		if f.addStatus != http.StatusOK {
			w.WriteHeader(f.addStatus)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"added_torrent_ids": []string{"abc123"},
			"success_count":     1,
		})
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
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
		name    string
		status  int
		wantErr bool
	}{
		{name: "success on 200 with json body", status: http.StatusOK},
		{name: "error on conflict", status: http.StatusConflict, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeQbit(t)
			fake.addStatus = tt.status

			err := fake.client().CreateDownloadTask("magnet:?xt=urn:btih:abc", "/downloads/movies")

			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "/downloads/movies", fake.addSavePath)
			assert.Equal(t, "magnet:?xt=urn:btih:abc", fake.addURL)
		})
	}
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
