// an in-package test cannot import app/http, which imports app/downloads, so the acceptance
// path is exercised from outside the package
package downloads_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"magnet-feed-sync/app/config"
	"magnet-feed-sync/app/database"
	"magnet-feed-sync/app/download-client/qbittorrent"
	download_store "magnet-feed-sync/app/download-store"
	"magnet-feed-sync/app/downloads"
	appHttp "magnet-feed-sync/app/http"
	"magnet-feed-sync/app/migrations"
	"magnet-feed-sync/app/notify"
	"magnet-feed-sync/app/tracker"
)

const (
	acceptanceHash     = "474d1403945c0768506233481557516e7af8d136"
	acceptanceMagnet   = "magnet:?xt=urn:btih:474d1403945c0768506233481557516e7af8d136&dn=sample.bin"
	acceptanceLocation = "/downloads/cinema-prep"
)

const completedTorrentBody = `[{"hash":"474d1403945c0768506233481557516e7af8d136","name":"sample.bin","state":"stalledUP",
  "progress":1,"completion_on":1786626099,"amount_left":0,
  "content_path":"/downloads/probe/sample.bin","save_path":"/downloads/probe",
  "size":4194304,"total_size":4194304,"added_on":1786626098,"eta":8640000}]`

var downloadIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

type recordingNotifier struct {
	mu       sync.Mutex
	messages []notify.Message
}

func (r *recordingNotifier) Publish(_ context.Context, m notify.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.messages = append(r.messages, m)

	return nil
}

func (r *recordingNotifier) Enabled() bool {
	return true
}

func (r *recordingNotifier) captured() []notify.Message {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]notify.Message(nil), r.messages...)
}

type acceptanceTaskCreator struct {
	qbt      *qbittorrent.Client
	file     *tracker.FileMetadata
	askedFor []bool
}

func (t *acceptanceTaskCreator) DownloadNow(_ context.Context, source, location string) (string, error) {
	return t.qbt.CreateDownloadTask(source, location)
}

func (t *acceptanceTaskCreator) CreateFromURL(_ context.Context, url, location string, notifyFlag bool) (*tracker.FileMetadata, error) {
	t.askedFor = append(t.askedFor, notifyFlag)

	file := *t.file
	file.OriginalUrl = url
	file.Location = location
	file.Notify = notifyFlag

	return &file, nil
}

func (t *acceptanceTaskCreator) RemoveTask(string) error { return nil }

func (t *acceptanceTaskCreator) UpdateTaskLocation(string, string) error { return nil }

func (t *acceptanceTaskCreator) CheckFileForUpdates(context.Context, string) {}

func (t *acceptanceTaskCreator) RefreshAll(context.Context) {}

type acceptanceEnv struct {
	baseURL  string
	repo     *download_store.Repository
	qbt      *qbittorrent.Client
	notifier *recordingNotifier
	creator  *acceptanceTaskCreator
}

func newAcceptanceEnv(t *testing.T) *acceptanceEnv {
	t.Helper()

	qbt := qbittorrent.NewClient(config.QBittorrentConfig{
		URL:         newQbitServer(t).URL,
		Username:    "admin",
		Password:    "adminpass",
		Destination: "/downloads/other",
	})

	env := &acceptanceEnv{
		repo:     newAcceptanceRepo(t),
		qbt:      qbt,
		notifier: &recordingNotifier{},
		creator: &acceptanceTaskCreator{
			qbt: qbt,
			file: &tracker.FileMetadata{
				ID:               "1234567",
				Name:             "Sample Release",
				Magnet:           acceptanceMagnet,
				LastComment:      "season 2",
				LastSyncAt:       time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC),
				TorrentUpdatedAt: time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC),
			},
		},
	}

	env.baseURL = startAcceptanceServer(t, &appHttp.ClientCtx{
		TaskCreator:    env.creator,
		DownloadClient: qbt,
		DownloadStore:  env.repo,
		TorrentLookup:  qbt,
		Notifier:       env.notifier,
	})

	return env
}

func (e *acceptanceEnv) sweeper() *downloads.Sweeper {
	return downloads.NewSweeper(downloads.SweeperDeps{
		Store:    e.repo,
		Torrents: e.qbt,
		Notifier: e.notifier,
	})
}

func newQbitServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := nethttp.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		nethttp.SetCookie(w, &nethttp.Cookie{Name: "SID", Value: "test-session"})
		w.WriteHeader(nethttp.StatusNoContent)
	})
	mux.HandleFunc("/api/v2/torrents/add", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"added_torrent_ids":[%q],"success_count":1,"failure_count":0,"pending_count":0}`,
			acceptanceHash)
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(completedTorrentBody))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

func newAcceptanceRepo(t *testing.T) *download_store.Repository {
	t.Helper()

	t.Chdir(t.TempDir())

	db, err := database.NewClient("test.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	_, err = migrations.Apply(db.DB())
	require.NoError(t, err)

	repo, err := download_store.NewRepository(db)
	require.NoError(t, err)

	return repo
}

func startAcceptanceServer(t *testing.T, clientCtx *appHttp.ClientCtx) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	clientCtx.Config = config.HttpConfig{Port: port}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go appHttp.NewClient(clientCtx).Start(runCtx, done)
	t.Cleanup(func() {
		cancel()
		<-done
	})

	address := fmt.Sprintf("127.0.0.1:%d", port)
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err != nil {
			return false
		}

		return conn.Close() == nil
	}, 5*time.Second, 20*time.Millisecond)

	return "http://" + address
}

func postJSON(t *testing.T, url string, body map[string]any) (int, map[string]any) {
	t.Helper()

	encoded, err := json.Marshal(body)
	require.NoError(t, err)

	resp, err := nethttp.Post(url, "application/json", bytes.NewReader(encoded))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded), "body was %s", raw)

	return resp.StatusCode, decoded
}

func TestAcceptanceMagnetNotifyToCompletedEvent(t *testing.T) {
	env := newAcceptanceEnv(t)

	code, body := postJSON(t, env.baseURL+"/api/downloads", map[string]any{
		"source":   acceptanceMagnet,
		"location": acceptanceLocation,
		"notify":   true,
	})

	require.Equal(t, nethttp.StatusCreated, code)
	assert.Equal(t, "ok", body["status"])
	downloadID, _ := body["download_id"].(string)
	require.Regexp(t, downloadIDPattern, downloadID)
	assert.Equal(t, "tuclaw.downloads.completed."+downloadID, body["subject"])

	pending, err := env.repo.Pending()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, acceptanceHash, pending[0].Hash)
	assert.Equal(t, acceptanceLocation, pending[0].Location)

	require.NoError(t, env.sweeper().RunCycle(context.Background()))

	messages := env.notifier.captured()
	require.Len(t, messages, 1)
	assert.Equal(t, "tuclaw.downloads.completed."+downloadID, messages[0].Subject)
	assert.Equal(t, downloadID+":completed", messages[0].MsgID)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(messages[0].Payload, &payload))
	assert.Equal(t, map[string]any{
		"download_id":  downloadID,
		"status":       "completed",
		"hash":         acceptanceHash,
		"name":         "sample.bin",
		"content_path": "/downloads/probe/sample.bin",
		"size":         float64(4194304),
		"location":     acceptanceLocation,
		"completed_at": time.Unix(1786626099, 0).UTC().Format(time.RFC3339),
	}, payload)

	stillPending, err := env.repo.Pending()
	require.NoError(t, err)
	assert.Empty(t, stillPending)

	require.NoError(t, env.sweeper().RunCycle(context.Background()))
	assert.Len(t, env.notifier.captured(), 1, "a published row must never be woken twice")
}

func TestAcceptanceDownloadWithoutNotifyIsUnchanged(t *testing.T) {
	env := newAcceptanceEnv(t)

	code, body := postJSON(t, env.baseURL+"/api/downloads", map[string]any{
		"source":   acceptanceMagnet,
		"location": acceptanceLocation,
	})

	require.Equal(t, nethttp.StatusCreated, code)
	assert.Equal(t, map[string]any{"status": "ok"}, body)

	count, err := env.repo.CountPending()
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.Empty(t, env.notifier.captured())
}

func TestAcceptanceFileWithoutNotifyGainsOnlyTheNotifyKey(t *testing.T) {
	env := newAcceptanceEnv(t)

	code, body := postJSON(t, env.baseURL+"/api/files", map[string]any{
		"url":      "https://rutracker.org/forum/viewtopic.php?t=1234567",
		"location": acceptanceLocation,
	})

	require.Equal(t, nethttp.StatusCreated, code)
	assert.Equal(t, map[string]any{
		"id":               "1234567",
		"originalUrl":      "https://rutracker.org/forum/viewtopic.php?t=1234567",
		"name":             "Sample Release",
		"lastComment":      "season 2",
		"lastSyncAt":       "2026-08-13T10:00:00Z",
		"magnet":           acceptanceMagnet,
		"torrentUpdatedAt": "2026-08-12T09:00:00Z",
		"location":         acceptanceLocation,
		"notify":           false,
	}, body)
	assert.Equal(t, []bool{false}, env.creator.askedFor)
}
