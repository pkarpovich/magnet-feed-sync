package download_tasks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"magnet-feed-sync/app/bot"
	"magnet-feed-sync/app/notify"
	taskStore "magnet-feed-sync/app/task-store"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/tracker/providers"
	"magnet-feed-sync/app/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockFileParser struct {
	parseFunc    func(url, location string) (*tracker.FileMetadata, error)
	providerName string
}

func (m *mockFileParser) Parse(_ context.Context, url, location string) (*tracker.FileMetadata, error) {
	return m.parseFunc(url, location)
}

func (m *mockFileParser) ProviderName(url string) string {
	return m.providerName
}

type mockBreaker struct {
	allowed   map[string]bool
	states    map[string]tracker.State
	begun     int
	failures  []string
	kinds     []providers.ErrorKind
	successes []string
}

func (m *mockBreaker) Allow(name string) bool {
	allowed, ok := m.allowed[name]
	return !ok || allowed
}

func (m *mockBreaker) BeginRun() {
	m.begun++
}

func (m *mockBreaker) RecordFailure(name string, kind providers.ErrorKind) {
	m.failures = append(m.failures, name)
	m.kinds = append(m.kinds, kind)
}

func (m *mockBreaker) RecordSuccess(name string) {
	m.successes = append(m.successes, name)
}

func (m *mockBreaker) Snapshot() map[string]tracker.State {
	return m.states
}

type mockFileStore struct {
	getByIdFunc         func(id string) (*tracker.FileMetadata, error)
	createOrReplaceFunc func(metadata *tracker.FileMetadata) error
	getAllFunc          func() ([]*tracker.FileMetadata, error)
	removeFunc          func(id string) error
	updateSettingsFunc  func(id string, notify bool, location string) error
	successes           []string
	failures            []taskStore.SyncFailure
	failureIds          []string
	runs                []bool
	setLastRunErr       error
	recordFailureErr    error
}

func (m *mockFileStore) GetById(id string) (*tracker.FileMetadata, error) {
	return m.getByIdFunc(id)
}

func (m *mockFileStore) CreateOrReplace(metadata *tracker.FileMetadata) error {
	return m.createOrReplaceFunc(metadata)
}

func (m *mockFileStore) GetAll() ([]*tracker.FileMetadata, error) {
	return m.getAllFunc()
}

func (m *mockFileStore) Remove(id string) error {
	return m.removeFunc(id)
}

func (m *mockFileStore) RecordSyncSuccess(id string, _ time.Time) error {
	m.successes = append(m.successes, id)
	return nil
}

func (m *mockFileStore) RecordSyncFailure(id string, failure taskStore.SyncFailure) error {
	if m.recordFailureErr != nil {
		return m.recordFailureErr
	}
	m.failureIds = append(m.failureIds, id)
	m.failures = append(m.failures, failure)
	return nil
}

func (m *mockFileStore) SetLastRun(_ time.Time, ok bool) error {
	m.runs = append(m.runs, ok)
	return m.setLastRunErr
}

func (m *mockFileStore) UpdateSettings(id string, notify bool, location string) error {
	if m.updateSettingsFunc == nil {
		return nil
	}
	return m.updateSettingsFunc(id, notify, location)
}

type mockDownloadClient struct {
	createDownloadTaskFunc func(url, destination string) error
	hash                   string
}

func (m *mockDownloadClient) CreateDownloadTask(url, destination string) (string, error) {
	return m.hash, m.createDownloadTaskFunc(url, destination)
}

func (m *mockDownloadClient) SetLocation(taskID, location string) error {
	return nil
}

func (m *mockDownloadClient) GetLocations() []types.Location {
	return nil
}

func (m *mockDownloadClient) GetHashByMagnet(magnet string) (string, error) {
	return "", nil
}

func (m *mockDownloadClient) GetDefaultLocation() string {
	return "/downloads"
}

func TestDownloadNow_DryMode_SkipsDownloadClient(t *testing.T) {
	downloadCalled := false
	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			downloadCalled = true
			return nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		DClient:         dClient,
		DryMode:         true,
	})

	hash, err := client.DownloadNow(context.Background(), "magnet:?xt=urn:btih:abc123", "/downloads")

	require.NoError(t, err)
	assert.Empty(t, hash, "dry mode adds no torrent, so there is no hash to report")
	assert.False(t, downloadCalled, "download client should not be called in dry mode")
}

func TestDownloadNow_ForwardsSourceAndLocation(t *testing.T) {
	var gotURL, gotDestination string
	callCount := 0
	dClient := &mockDownloadClient{
		hash: "474d1403945c0768506233481557516e7af8d136",
		createDownloadTaskFunc: func(url, destination string) error {
			callCount++
			gotURL = url
			gotDestination = destination
			return nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		DClient:         dClient,
		DryMode:         false,
	})

	source := "https://jackett.example/dl/tpb/torrent.torrent?apikey=secret"
	hash, err := client.DownloadNow(context.Background(), source, "/downloads/movies")

	require.NoError(t, err)
	assert.Equal(t, "474d1403945c0768506233481557516e7af8d136", hash, "the add response hash is what identifies the row")
	assert.Equal(t, 1, callCount, "download client should be called exactly once")
	assert.Equal(t, source, gotURL, "source should be forwarded verbatim")
	assert.Equal(t, "/downloads/movies", gotDestination, "location should be forwarded verbatim")
}

func TestDownloadNow_PropagatesError(t *testing.T) {
	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			return fmt.Errorf("qbittorrent unavailable")
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		DClient:         dClient,
		DryMode:         false,
	})

	_, err := client.DownloadNow(context.Background(), "magnet:?xt=urn:btih:abc123", "/downloads")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "qbittorrent unavailable")
}

// pins the intermediate state: a duplicate add is still an ordinary error here, which is what
// makes POST /api/downloads answer 500 for it until task 6 resolves the duplicate instead
func TestDownloadNow_PropagatesAlreadyExists(t *testing.T) {
	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			return fmt.Errorf("add torrent: %w", types.ErrTorrentAlreadyExists)
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		DClient:         dClient,
	})

	_, err := client.DownloadNow(context.Background(), "magnet:?xt=urn:btih:abc123", "/downloads")

	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrTorrentAlreadyExists))
}

func TestProcessFileMetadata_SameMagnetDifferentDate_NoRedownload(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:abc123"
	oldDate := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	newDate := time.Date(2026, 3, 22, 12, 59, 0, 0, time.UTC)

	var savedMetadata *tracker.FileMetadata
	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               "3304959",
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=3304959",
				Magnet:           magnet,
				Name:             "Test Torrent",
				TorrentUpdatedAt: oldDate,
				Location:         "/downloads",
			}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			savedMetadata = metadata
			return nil
		},
	}

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               "3304959",
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=3304959",
				Magnet:           magnet,
				Name:             "Test Torrent (edited description)",
				TorrentUpdatedAt: newDate,
			}, nil
		},
	}

	downloadCalled := false
	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			downloadCalled = true
			return nil
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      magnet,
	}, true)

	assert.False(t, downloadCalled, "download should not be triggered when magnet is unchanged")
	assert.Empty(t, msgChan, "no notification should be sent when magnet is unchanged")
	require.NotNil(t, savedMetadata, "metadata should be saved to store")
	assert.Equal(t, newDate, savedMetadata.TorrentUpdatedAt, "date should be updated in DB")
	assert.Equal(t, "Test Torrent (edited description)", savedMetadata.Name, "name should be updated in DB")
	assert.Equal(t, "/downloads", savedMetadata.Location, "location should be preserved")
}

func TestProcessFileMetadata_DifferentMagnet_RedownloadTriggered(t *testing.T) {
	oldMagnet := "magnet:?xt=urn:btih:abc123"
	newMagnet := "magnet:?xt=urn:btih:def456"
	oldDate := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	newDate := time.Date(2026, 3, 22, 12, 59, 0, 0, time.UTC)

	var savedMetadata *tracker.FileMetadata
	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               "3304959",
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=3304959",
				Magnet:           oldMagnet,
				Name:             "Test Torrent",
				TorrentUpdatedAt: oldDate,
				Location:         "/downloads",
			}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			savedMetadata = metadata
			return nil
		},
	}

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               "3304959",
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=3304959",
				Magnet:           newMagnet,
				Name:             "Test Torrent v2",
				TorrentUpdatedAt: newDate,
			}, nil
		},
	}

	downloadCalled := false
	var downloadedMagnet string
	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			downloadCalled = true
			downloadedMagnet = url
			return nil
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      oldMagnet,
	}, true)

	assert.True(t, downloadCalled, "download should be triggered when magnet changes")
	assert.Equal(t, newMagnet, downloadedMagnet, "new magnet should be used for download")
	require.NotNil(t, savedMetadata, "metadata should be saved to store")
	assert.Equal(t, newMagnet, savedMetadata.Magnet, "new magnet should be stored")
	assert.Equal(t, "Test Torrent v2", savedMetadata.Name, "new name should be stored")
	assert.Equal(t, "/downloads", savedMetadata.Location, "location should be preserved")

	select {
	case msg := <-msgChan:
		assert.Contains(t, msg, "Metadata updated")
	default:
		t.Fatal("notification should be sent when magnet changes")
	}
}

func TestProcessFileMetadata_SameMagnetSameDate_MetadataUpdated(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:abc123"
	date := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)

	var savedMetadata *tracker.FileMetadata
	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               "3304959",
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=3304959",
				Magnet:           magnet,
				Name:             "Test Torrent",
				TorrentUpdatedAt: date,
				Location:         "/downloads",
			}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			savedMetadata = metadata
			return nil
		},
	}

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               "3304959",
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=3304959",
				Magnet:           magnet,
				Name:             "Test Torrent",
				TorrentUpdatedAt: date,
			}, nil
		},
	}

	downloadCalled := false
	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			downloadCalled = true
			return nil
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      magnet,
	}, true)

	assert.False(t, downloadCalled, "download should not be triggered")
	assert.Empty(t, msgChan, "no notification should be sent")
	require.NotNil(t, savedMetadata, "metadata should still be saved (last_sync_at updated)")
}

func TestProcessFileMetadata_ParseError_NoCrash(t *testing.T) {
	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, fmt.Errorf("network error: connection refused")
		},
	}

	store := &mockFileStore{}
	dClient := &mockDownloadClient{}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, true)

	assert.Empty(t, msgChan, "no notification should be sent on parse error")
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

func TestProcessFileMetadata_ParseError_LogsTaskIdAndUrl(t *testing.T) {
	logs := captureLogs(t)

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, fmt.Errorf("network error: connection refused")
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           &mockFileStore{},
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, true)

	var line map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &line))
	assert.Equal(t, "error parsing metadata", line["msg"])
	assert.Equal(t, "3304959", line["id"])
	assert.Equal(t, "https://rutracker.org/forum/viewtopic.php?t=3304959", line["url"])
	assert.Equal(t, "network error: connection refused", line["error"])
}

func TestCheckFileForUpdates_StoreError_LogsTaskId(t *testing.T) {
	logs := captureLogs(t)

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return nil, fmt.Errorf("no such row")
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         &mockFileParser{},
		DClient:         &mockDownloadClient{},
		Store:           store,
	})

	client.CheckFileForUpdates(context.Background(), "3304959")

	var line map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &line))
	assert.Equal(t, "error getting metadata", line["msg"])
	assert.Equal(t, "3304959", line["id"])
	assert.Equal(t, "no such row", line["error"])
}

func TestProcessFileMetadata_EmptyOriginalUrl_Skipped(t *testing.T) {
	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			t.Fatal("parser should not be called for empty URL")
			return nil, nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		Store:           &mockFileStore{},
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "",
	}, true)
}

func TestProcessFileMetadata_DeletedTask_Skipped(t *testing.T) {
	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:     "3304959",
				Magnet: "magnet:?xt=urn:btih:new",
			}, nil
		},
	}

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:       "3304959",
				Magnet:   "magnet:?xt=urn:btih:old",
				DeleteAt: sql.NullTime{Time: time.Now(), Valid: true},
			}, nil
		},
	}

	downloadCalled := false
	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			downloadCalled = true
			return nil
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, true)

	assert.False(t, downloadCalled, "download should not be triggered for deleted task")
	assert.Empty(t, msgChan, "no notification for deleted task")
}

func TestProcessFileMetadata_DifferentMagnet_DryMode_NoDownload(t *testing.T) {
	oldMagnet := "magnet:?xt=urn:btih:abc123"
	newMagnet := "magnet:?xt=urn:btih:def456"

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:       "3304959",
				Magnet:   oldMagnet,
				Location: "/downloads",
			}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			return nil
		},
	}

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:     "3304959",
				Magnet: newMagnet,
			}, nil
		},
	}

	downloadCalled := false
	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			downloadCalled = true
			return nil
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         true,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      oldMagnet,
	}, true)

	assert.False(t, downloadCalled, "download should not be triggered in dry mode")

	select {
	case msg := <-msgChan:
		assert.Contains(t, msg, "Metadata updated")
	default:
		t.Fatal("notification should still be sent in dry mode when magnet changes")
	}
}

func TestProcessFileMetadata_DifferentMagnet_DownloadFails_MagnetReverted(t *testing.T) {
	oldMagnet := "magnet:?xt=urn:btih:abc123"
	newMagnet := "magnet:?xt=urn:btih:def456"
	oldDate := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	newDate := time.Date(2026, 3, 22, 12, 59, 0, 0, time.UTC)

	var lastSavedMetadata *tracker.FileMetadata
	saveCount := 0
	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               "3304959",
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=3304959",
				Magnet:           oldMagnet,
				Name:             "Test Torrent",
				TorrentUpdatedAt: oldDate,
				Location:         "/downloads",
			}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			saveCount++
			lastSavedMetadata = &tracker.FileMetadata{
				ID:               metadata.ID,
				Magnet:           metadata.Magnet,
				TorrentUpdatedAt: metadata.TorrentUpdatedAt,
				Location:         metadata.Location,
				Name:             metadata.Name,
			}
			return nil
		},
	}

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               "3304959",
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=3304959",
				Magnet:           newMagnet,
				Name:             "Test Torrent v2",
				TorrentUpdatedAt: newDate,
			}, nil
		},
	}

	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			return fmt.Errorf("download station unavailable")
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      oldMagnet,
	}, true)

	assert.Equal(t, 2, saveCount, "store should be written twice: update then rollback")
	require.NotNil(t, lastSavedMetadata, "rollback metadata should be saved")
	assert.Equal(t, oldMagnet, lastSavedMetadata.Magnet, "magnet should be reverted to original")
	assert.Equal(t, oldDate, lastSavedMetadata.TorrentUpdatedAt, "torrent_updated_at should be reverted")
}

func TestProcessFileMetadata_SameBtihDifferentTrackerUrl_NoRedownload(t *testing.T) {
	storedMagnet := "magnet:?xt=urn:btih:ABC123&tr=http://bt3.t-ru.org/ann"
	parsedMagnet := "magnet:?xt=urn:btih:abc123&tr=http://bt4.t-ru.org/ann"

	downloadCalled := false
	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:       "3304959",
				Magnet:   storedMagnet,
				Location: "/downloads",
			}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			return nil
		},
	}

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:     "3304959",
				Magnet: parsedMagnet,
			}, nil
		},
	}

	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			downloadCalled = true
			return nil
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, true)

	assert.False(t, downloadCalled, "download should not trigger when btih hash matches despite different tracker URLs")
	assert.Empty(t, msgChan, "no notification when btih hash matches")
}

func TestProcessFileMetadata_NoBtihHash_DifferentMagnets_RedownloadTriggered(t *testing.T) {
	storedMagnet := "magnet:?xt=urn:btmh:1220abc123"
	parsedMagnet := "magnet:?xt=urn:btmh:1220def456"

	downloadCalled := false
	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:       "test-v2",
				Magnet:   storedMagnet,
				Location: "/downloads",
			}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			return nil
		},
	}

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:     "test-v2",
				Magnet: parsedMagnet,
			}, nil
		},
	}

	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			downloadCalled = true
			return nil
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		DryMode:         false,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "test-v2",
		OriginalUrl: "https://example.com/topic/123",
	}, true)

	assert.True(t, downloadCalled, "download should trigger when magnets differ and have no btih hash")
}

func TestProcessFileMetadata_BlockedError_RecordsBreakerFailure(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	breaker := &mockBreaker{}
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           &mockFileStore{},
		Breaker:         breaker,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, true)

	assert.Equal(t, []string{"rutracker"}, breaker.failures)
	assert.Equal(t, []providers.ErrorKind{providers.KindBlocked}, breaker.kinds)
	assert.Empty(t, breaker.successes)
}

func TestProcessFileMetadata_UnclassifiedError_RecordsNothing(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	breaker := &mockBreaker{}
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           &mockFileStore{},
		Breaker:         breaker,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, true)

	assert.Empty(t, breaker.failures, "an unclassified error must not feed the breaker")
}

func TestProcessFileMetadata_ParseSuccess_RecordsBreakerSuccess(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:abc123"
	parser := &mockFileParser{
		providerName: "nnm",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "1", Magnet: magnet}, nil
		},
	}

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "1", Magnet: magnet, Location: "/downloads"}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error { return nil },
	}

	breaker := &mockBreaker{}
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         breaker,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "1",
		OriginalUrl: "https://nnmclub.to/forum/viewtopic.php?t=1",
	}, true)

	assert.Equal(t, []string{"nnm"}, breaker.successes)
}

func TestCheckForUpdates_BlockedProvider_SkipsWithoutParse(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			t.Fatal("parser must not be called for a blocked provider")
			return nil, nil
		},
	}

	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return []*tracker.FileMetadata{
				{ID: "1", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=1"},
				{ID: "2", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=2"},
			}, nil
		},
	}

	breaker := &mockBreaker{allowed: map[string]bool{"rutracker": false}}
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         breaker,
	})

	client.CheckForUpdates(context.Background())

	assert.Equal(t, 1, breaker.begun, "the run must be announced to the breaker exactly once")
	assert.Empty(t, breaker.failures, "skipped tasks must not record anything")
}

func TestParseFailureIncrements(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	store := &mockFileStore{}
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         &mockBreaker{},
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, true)

	require.Len(t, store.failures, 1, "a parse failure must be recorded once")
	assert.Equal(t, []string{"3304959"}, store.failureIds)
	assert.Equal(t, "Blocked: challenge", store.failures[0].Text)
	assert.False(t, store.failures[0].At.IsZero(), "failure time must be set")
	assert.Empty(t, store.successes, "a parse failure must not record a success")
}

func TestDownloadFailureLeavesCounterZero(t *testing.T) {
	oldMagnet := "magnet:?xt=urn:btih:abc123"
	newMagnet := "magnet:?xt=urn:btih:def456"

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "3304959", Magnet: oldMagnet, Location: "/downloads"}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error { return nil },
	}

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "3304959", Magnet: newMagnet}, nil
		},
	}

	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			return fmt.Errorf("qbittorrent unavailable")
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      oldMagnet,
	}, true)

	assert.Empty(t, store.failures, "a download client error must not count as a task failure")
	assert.Equal(t, []string{"3304959"}, store.successes, "the successful parse must still be recorded")
}

func TestStoreErrorLeavesCounterZero(t *testing.T) {
	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "3304959", Magnet: "magnet:?xt=urn:btih:abc123"}, nil
		},
	}

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return nil, fmt.Errorf("database is locked")
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, true)

	assert.Empty(t, store.failures, "a store error must not count as a task failure")
	assert.Equal(t, []string{"3304959"}, store.successes)
}

func TestManualRefreshDoesNotTripBreaker(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:          "3304959",
				OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
			}, nil
		},
	}

	breaker := &mockBreaker{}
	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         breaker,
	})

	client.CheckFileForUpdates(context.Background(), "3304959")

	assert.Empty(t, breaker.failures, "a manual refresh must not trip the breaker")
	require.Len(t, store.failures, 1, "a manual refresh must still keep the counter truthful")
	assert.Equal(t, "Blocked: challenge", store.failures[0].Text)
	assert.Empty(t, msgChan, "a manual refresh must not notify")
}

// RefreshAll is the web app's refresh button; recording it as a cron run would let a human
// hide a dead cron from the health endpoint's staleness check
func TestRefreshAllDoesNotActAsCronRun(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return []*tracker.FileMetadata{
				{ID: "1", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=1", ConsecutiveFailures: FailureThreshold - 1},
			}, nil
		},
	}

	breaker := &mockBreaker{}
	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         breaker,
	})

	client.RefreshAll(context.Background())

	assert.Empty(t, store.runs, "a manual refresh must not overwrite the cron run state")
	assert.Empty(t, breaker.failures, "a manual refresh must not trip the breaker")
	assert.Empty(t, msgChan, "a manual refresh must not notify")
	require.Len(t, store.failures, 1, "a manual refresh must still keep the counter truthful")
}

// the stretch and the breaker gate exist to spare a broken tracker on the hourly sweep; the
// refresh button is an explicit "retry now", so it goes through like the per-file refresh
func TestRefreshAllRetriesStretchedAndBlockedTasks(t *testing.T) {
	parseCalls := 0
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			parseCalls++
			return nil, fmt.Errorf("still down")
		},
	}
	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return []*tracker.FileMetadata{
				{
					ID:                  "1",
					OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=1",
					ConsecutiveFailures: FailureThreshold,
					LastErrorAt:         sql.NullTime{Time: time.Now(), Valid: true},
				},
			}, nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         &mockBreaker{allowed: map[string]bool{"rutracker": false}},
	})

	client.RefreshAll(context.Background())

	assert.Equal(t, 1, parseCalls)
}

func TestNotifyOnceAtThreshold(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           &mockFileStore{},
		Breaker:         &mockBreaker{},
	})

	for failures := 0; failures < 5; failures++ {
		client.processFileMetadata(context.Background(), &tracker.FileMetadata{
			ID:                  "3304959",
			Name:                "Some Movie",
			OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=3304959",
			ConsecutiveFailures: failures,
		}, true)
	}

	require.Len(t, msgChan, 1, "only the run that crosses the threshold notifies")
	msg := <-msgChan
	assert.Contains(t, msg, "Some Movie")
	assert.Contains(t, msg, "3304959")
	assert.Contains(t, msg, "Blocked: challenge")
}

// the alert fires once per streak and is claimed for good, so a run whose counter write
// failed must not spend it: the store still says 2, and the real crossing comes later
func TestNoNotifyWhenFailureNotRecorded(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	store := &mockFileStore{recordFailureErr: fmt.Errorf("database is locked")}
	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         &mockBreaker{},
	})

	metadata := &tracker.FileMetadata{
		ID:                  "3304959",
		Name:                "Some Movie",
		OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=3304959",
		ConsecutiveFailures: FailureThreshold - 1,
	}

	client.processFileMetadata(context.Background(), metadata, true)
	require.Empty(t, msgChan, "the counter never moved, so there is nothing to alert about")

	store.recordFailureErr = nil
	client.processFileMetadata(context.Background(), metadata, true)

	require.Len(t, msgChan, 1, "the alert slot has to survive the failed write")
	assert.Contains(t, <-msgChan, "Some Movie")
}

// a deleted row is off every list the health endpoint reads, so letting a refresh keep
// scoring it would leave counters nobody can clear
func TestRefreshSkipsDeletedTask(t *testing.T) {
	parseCalls := 0
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			parseCalls++
			return nil, fmt.Errorf("boom")
		},
	}

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:          id,
				OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=1",
				DeleteAt:    sql.NullTime{Time: time.Now(), Valid: true},
			}, nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         &mockBreaker{},
	})

	client.CheckFileForUpdates(context.Background(), "1")

	assert.Zero(t, parseCalls)
	assert.Empty(t, store.failures)
}

// a manual refresh increments the counter without notifying, so the run that crosses the
// threshold may not be a cron run at all — keying the alert off that exact transition lost it
func TestNotifyAfterManualRefreshCrossedThreshold(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           &mockFileStore{},
		Breaker:         &mockBreaker{},
	})

	task := func(failures int) *tracker.FileMetadata {
		return &tracker.FileMetadata{
			ID:                  "3304959",
			Name:                "Some Movie",
			OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=3304959",
			ConsecutiveFailures: failures,
		}
	}

	// the manual refresh performs the 2 -> 3 increment silently
	client.processFileMetadata(context.Background(), task(FailureThreshold-1), false)
	require.Empty(t, msgChan, "a manual refresh must not notify")

	client.processFileMetadata(context.Background(), task(FailureThreshold), true)
	require.Len(t, msgChan, 1, "the first cron run to see the task failing still alerts")
	assert.Contains(t, <-msgChan, "Some Movie")

	client.processFileMetadata(context.Background(), task(FailureThreshold+1), true)
	assert.Empty(t, msgChan, "the alert stays a one-shot for the streak")
}

// the notice is cleared on any recorded success, including a manual refresh's, or a later
// streak on the same task would never alert again
func TestNotifyAgainAfterRecovery(t *testing.T) {
	parseErr := error(nil)
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			if parseErr != nil {
				return nil, parseErr
			}
			return &tracker.FileMetadata{ID: "3304959", Magnet: "magnet:?xt=urn:btih:aaa"}, nil
		},
	}

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: id, Magnet: "magnet:?xt=urn:btih:aaa"}, nil
		},
		createOrReplaceFunc: func(*tracker.FileMetadata) error { return nil },
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         &mockBreaker{},
	})

	failing := &tracker.FileMetadata{
		ID:                  "3304959",
		Name:                "Some Movie",
		OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=3304959",
		ConsecutiveFailures: FailureThreshold - 1,
	}

	parseErr = &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
	client.processFileMetadata(context.Background(), failing, true)
	require.Len(t, msgChan, 1)
	<-msgChan

	// a manual refresh succeeds, which resets the counter in the store
	parseErr = nil
	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	}, false)
	require.Equal(t, []string{"3304959"}, store.successes)

	parseErr = &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
	client.processFileMetadata(context.Background(), failing, true)
	assert.Len(t, msgChan, 1, "a new streak alerts again")
}

// re-adding a tracked task rewrites the row with a freshly parsed metadata, zeroing the
// failure counters; the in-memory notice has to be dropped with them or the next streak
// on that task would stay silent forever
func TestNotifyAgainAfterTaskRecreated(t *testing.T) {
	parseErr := error(nil)
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			if parseErr != nil {
				return nil, parseErr
			}
			return &tracker.FileMetadata{ID: "3304959", Magnet: "magnet:?xt=urn:btih:aaa"}, nil
		},
	}

	store := &mockFileStore{
		getByIdFunc:         func(id string) (*tracker.FileMetadata, error) { return nil, sql.ErrNoRows },
		createOrReplaceFunc: func(*tracker.FileMetadata) error { return nil },
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{createDownloadTaskFunc: func(string, string) error { return nil }},
		Store:           store,
		Breaker:         &mockBreaker{},
	})

	failing := &tracker.FileMetadata{
		ID:                  "3304959",
		Name:                "Some Movie",
		OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=3304959",
		ConsecutiveFailures: FailureThreshold - 1,
	}

	parseErr = &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
	client.processFileMetadata(context.Background(), failing, true)
	require.Len(t, msgChan, 1)
	<-msgChan

	// the user re-adds the url; the parse succeeds and the counters go back to zero
	parseErr = nil
	_, err := client.CreateFromURL(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=3304959", "/movies", false)
	require.NoError(t, err)

	parseErr = &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
	client.processFileMetadata(context.Background(), failing, true)
	assert.Len(t, msgChan, 1, "a streak after a re-create alerts again")
}

func TestNotificationsEscapeMarkdown(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("bad status: 403 (Forbidden)")}
		},
	}

	msgChan := make(chan string, 10)
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           &mockFileStore{},
		Breaker:         &mockBreaker{},
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:                  "3304959",
		Name:                "Some.Movie (2024) [1080p]",
		OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=3304959",
		ConsecutiveFailures: FailureThreshold - 1,
	}, true)

	require.Len(t, msgChan, 1)
	msg := <-msgChan
	assert.Contains(t, msg, `Some\.Movie \(2024\) \[1080p\]`)
	assert.Contains(t, msg, `403 \(Forbidden\)`)
	assert.NotRegexp(t, `[^\\][()\[\].!-]`, msg, "every reserved MarkdownV2 char must be escaped")
}

func TestEscapeMarkdownEscapesBackslash(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lone backslash", in: `AC\DC`, want: `AC\\DC`},
		{name: "trailing backslash", in: `C:\dir\`, want: `C:\\dir\\`},
		{name: "backslash before reserved char", in: `a\*b`, want: `a\\\*b`},
		{name: "escaped quote from an error string", in: `unexpected \"eof\"`, want: `unexpected \\"eof\\"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, escapeMarkdown(tt.in))
		})
	}
}

func TestNotifyOnceOnRecovery(t *testing.T) {
	tests := []struct {
		name       string
		failures   int
		wantNotify bool
	}{
		{name: "recovery from failing", failures: FailureThreshold, wantNotify: true},
		{name: "recovery during ramp-up is silent", failures: FailureThreshold - 1, wantNotify: false},
	}

	magnet := "magnet:?xt=urn:btih:abc123"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := &mockFileParser{
				providerName: "rutracker",
				parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
					return &tracker.FileMetadata{ID: "3304959", Magnet: magnet}, nil
				},
			}

			store := &mockFileStore{
				getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
					return &tracker.FileMetadata{ID: "3304959", Magnet: magnet, Location: "/downloads"}, nil
				},
				createOrReplaceFunc: func(metadata *tracker.FileMetadata) error { return nil },
			}

			msgChan := make(chan string, 10)
			client := NewClient(&ClientCtx{
				MessagesForSend: msgChan,
				Tracker:         parser,
				DClient:         &mockDownloadClient{},
				Store:           store,
				Breaker:         &mockBreaker{},
			})

			client.processFileMetadata(context.Background(), &tracker.FileMetadata{
				ID:                  "3304959",
				Name:                "Some Movie",
				OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=3304959",
				ConsecutiveFailures: tt.failures,
				LastError:           "Blocked: challenge",
			}, true)

			if !tt.wantNotify {
				assert.Empty(t, msgChan)
				return
			}

			require.Len(t, msgChan, 1)
			msg := <-msgChan
			assert.Contains(t, msg, "Some Movie")
			assert.Contains(t, msg, "3304959")
			assert.Contains(t, msg, "Blocked: challenge")
		})
	}
}

func TestBreakerNotifiesOncePerProvider(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return []*tracker.FileMetadata{
				{ID: "1", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=1"},
				{ID: "2", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=2"},
				{ID: "3", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3"},
			}, nil
		},
	}

	msgChan := make(chan string, 10)
	breaker := tracker.NewBreaker(nil, "rutracker")
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         breaker,
	})

	client.CheckForUpdates(context.Background())

	require.Len(t, msgChan, 1, "a tripped provider notifies once, not once per skipped task")
	msg := <-msgChan
	assert.Contains(t, msg, "rutracker")
	assert.Contains(t, msg, `2 task\(s\) skipped`, "reserved MarkdownV2 chars must be escaped")

	client.CheckForUpdates(context.Background())
	assert.Empty(t, msgChan, "an already blocked provider does not re-notify")
}

func TestCheckForUpdatesStopsOnCanceledContext(t *testing.T) {
	parsed := 0
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			parsed++
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("context canceled")}
		},
	}

	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return []*tracker.FileMetadata{
				{ID: "1", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=1"},
				{ID: "2", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=2"},
			}, nil
		},
	}

	msgChan := make(chan string, 10)
	breaker := &mockBreaker{}
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         breaker,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.CheckForUpdates(ctx)

	assert.Zero(t, parsed, "a canceled sweep parses nothing")
	assert.Empty(t, store.failureIds, "shutdown must not count as a task failure")
	assert.Empty(t, breaker.failures, "shutdown must not trip the breaker")
	assert.Empty(t, store.runs, "an interrupted sweep must not overwrite the run state")
	assert.Empty(t, msgChan)
}

func TestProcessFileMetadataIgnoresCanceledContext(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: context.Canceled}
		},
	}

	store := &mockFileStore{}
	msgChan := make(chan string, 10)
	breaker := &mockBreaker{}
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         breaker,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.processFileMetadata(ctx, &tracker.FileMetadata{
		ID:                  "1",
		OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=1",
		ConsecutiveFailures: FailureThreshold - 1,
	}, true)

	assert.Empty(t, store.failureIds)
	assert.Empty(t, breaker.failures)
	assert.Empty(t, msgChan)
}

func TestBreakerNotifiesOnRecovery(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:abc123"
	blocked := true
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			if blocked {
				return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
			}
			return &tracker.FileMetadata{ID: "1", Magnet: magnet}, nil
		},
	}

	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return []*tracker.FileMetadata{{ID: "1", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=1"}}, nil
		},
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "1", Magnet: magnet, Location: "/downloads"}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error { return nil },
	}

	now := time.Now()
	msgChan := make(chan string, 10)
	breaker := tracker.NewBreaker(func() time.Time { return now }, "rutracker")
	client := NewClient(&ClientCtx{
		MessagesForSend: msgChan,
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
		Breaker:         breaker,
	})

	client.CheckForUpdates(context.Background())
	require.Empty(t, msgChan, "one blocked fetch is not a trip, so there is nothing to announce")

	now = now.Add(time.Hour)
	client.CheckForUpdates(context.Background())
	require.Len(t, msgChan, 1, "the second blocked fetch in a row trips and announces it")
	<-msgChan

	blocked = false
	now = now.Add(2 * time.Hour)
	client.CheckForUpdates(context.Background())

	require.Len(t, msgChan, 1, "a successful half-open probe notifies once")
	assert.Contains(t, <-msgChan, "rutracker is reachable again")
}

func TestCheckForUpdates_FailingTask_SkippedWithinDeadInterval(t *testing.T) {
	tests := []struct {
		name        string
		failures    int
		lastErrorAt time.Time
		wantParsed  bool
	}{
		{name: "failing within interval", failures: FailureThreshold, lastErrorAt: time.Now().Add(-time.Hour), wantParsed: false},
		{name: "failing after interval", failures: FailureThreshold, lastErrorAt: time.Now().Add(-25 * time.Hour), wantParsed: true},
		{name: "below threshold", failures: FailureThreshold - 1, lastErrorAt: time.Now().Add(-time.Hour), wantParsed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := false
			parser := &mockFileParser{
				parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
					parsed = true
					return nil, fmt.Errorf("still broken")
				},
			}

			store := &mockFileStore{
				getAllFunc: func() ([]*tracker.FileMetadata, error) {
					return []*tracker.FileMetadata{{
						ID:                  "3304959",
						OriginalUrl:         "https://rutracker.org/forum/viewtopic.php?t=3304959",
						ConsecutiveFailures: tt.failures,
						LastErrorAt:         sql.NullTime{Time: tt.lastErrorAt, Valid: true},
					}}, nil
				},
			}

			client := NewClient(&ClientCtx{
				MessagesForSend: make(chan string, 10),
				Tracker:         parser,
				DClient:         &mockDownloadClient{},
				Store:           store,
			})

			client.CheckForUpdates(context.Background())

			assert.Equal(t, tt.wantParsed, parsed)
		})
	}
}

func TestMagnetsEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        string
		b        string
		expected bool
	}{
		{"same btih different tracker", "magnet:?xt=urn:btih:ABC123&tr=http://a.com", "magnet:?xt=urn:btih:abc123&tr=http://b.com", true},
		{"different btih", "magnet:?xt=urn:btih:abc123", "magnet:?xt=urn:btih:def456", false},
		{"no btih same magnet", "magnet:?xt=urn:btmh:1220abc", "magnet:?xt=urn:btmh:1220abc", true},
		{"no btih different magnet", "magnet:?xt=urn:btmh:1220abc", "magnet:?xt=urn:btmh:1220def", false},
		{"same btmh different tracker", "magnet:?xt=urn:btmh:1220abc&tr=http://a.com", "magnet:?xt=urn:btmh:1220abc&tr=http://b.com", true},
		{"one has btih other doesnt", "magnet:?xt=urn:btih:abc123", "magnet:?xt=urn:btmh:1220abc", false},
		{"both empty", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, magnetsEqual(tt.a, tt.b))
		})
	}
}

func setupTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	orig := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(orig)
	})
	return exporter
}

func TestProcessFileMetadata_CreatesTracingSpan(t *testing.T) {
	exporter := setupTestTracer(t)

	parser := &mockFileParser{
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:     "123",
				Magnet: "magnet:?xt=urn:btih:abc123",
			}, nil
		},
	}

	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:       "123",
				Magnet:   "magnet:?xt=urn:btih:abc123",
				Location: "/downloads",
			}, nil
		},
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			return nil
		},
	}

	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(url, destination string) error {
			return nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
	})

	client.processFileMetadata(context.Background(), &tracker.FileMetadata{
		ID:          "123",
		OriginalUrl: "https://example.com/topic/123",
	}, true)

	spans := exporter.GetSpans()
	require.GreaterOrEqual(t, len(spans), 1)

	spanNames := make([]string, len(spans))
	for i, s := range spans {
		spanNames[i] = s.Name
	}
	assert.Contains(t, spanNames, "processFileMetadata")
}

func TestCheckForUpdates_CreatesTracingSpan(t *testing.T) {
	exporter := setupTestTracer(t)

	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return nil, nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Store:           store,
	})

	client.CheckForUpdates(context.Background())

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "CheckForUpdates", spans[0].Name)
}

func TestCheckForUpdates_NoopTracingNoCrash(t *testing.T) {
	otel.SetTracerProvider(otel.GetTracerProvider())

	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return nil, nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Store:           store,
	})

	client.CheckForUpdates(context.Background())
}

func TestCheckForUpdates_RecordsRunOutcome(t *testing.T) {
	tests := []struct {
		name       string
		getAllFunc func() ([]*tracker.FileMetadata, error)
		wantOk     bool
	}{
		{
			name:       "sweep completed",
			getAllFunc: func() ([]*tracker.FileMetadata, error) { return nil, nil },
			wantOk:     true,
		},
		{
			name:       "sweep could not start",
			getAllFunc: func() ([]*tracker.FileMetadata, error) { return nil, fmt.Errorf("db is locked") },
			wantOk:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &mockFileStore{getAllFunc: tt.getAllFunc}
			client := NewClient(&ClientCtx{
				MessagesForSend: make(chan string, 10),
				Tracker:         &mockFileParser{},
				DClient:         &mockDownloadClient{},
				Store:           store,
			})

			client.CheckForUpdates(context.Background())

			require.Equal(t, []bool{tt.wantOk}, store.runs)
		})
	}
}

func TestCheckForUpdates_TaskFailureKeepsRunOk(t *testing.T) {
	parser := &mockFileParser{
		providerName: "rutracker",
		parseFunc: func(url, location string) (*tracker.FileMetadata, error) {
			return nil, &providers.ProviderError{Kind: providers.KindBlocked, Err: fmt.Errorf("challenge")}
		},
	}

	store := &mockFileStore{
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return []*tracker.FileMetadata{
				{ID: "1", OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=1"},
			}, nil
		},
	}

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{},
		Store:           store,
	})

	client.CheckForUpdates(context.Background())

	require.Equal(t, []bool{true}, store.runs)
	require.Len(t, store.failureIds, 1)
}

type recordingNotifier struct {
	messages []notify.Message
	err      error
}

func (r *recordingNotifier) Publish(_ context.Context, m notify.Message) error {
	r.messages = append(r.messages, m)
	return r.err
}

const (
	releaseOldMagnet = "magnet:?xt=urn:btih:abc123"
	releaseNewMagnet = "magnet:?xt=urn:btih:def456"
)

type releaseScenario struct {
	fileID       string
	storedNotify bool
	newMagnet    string
	dryMode      bool
	location     string
	downloadErr  error
}

type releaseRun struct {
	client   *Client
	notifier *recordingNotifier
	saved    func() *tracker.FileMetadata
	stored   *tracker.FileMetadata
}

func newReleaseRun(s releaseScenario) releaseRun {
	id := s.fileID
	if id == "" {
		id = "3304959"
	}
	newMagnet := s.newMagnet
	if newMagnet == "" {
		newMagnet = releaseNewMagnet
	}

	stored := &tracker.FileMetadata{
		ID:               id,
		OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=" + id,
		Magnet:           releaseOldMagnet,
		Name:             "Test Torrent",
		LastComment:      "old comment",
		Location:         s.location,
		Notify:           s.storedNotify,
		TorrentUpdatedAt: time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC),
	}

	var saved *tracker.FileMetadata
	store := &mockFileStore{
		getByIdFunc: func(string) (*tracker.FileMetadata, error) { return stored, nil },
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			copied := *metadata
			saved = &copied
			return nil
		},
		getAllFunc: func() ([]*tracker.FileMetadata, error) {
			return []*tracker.FileMetadata{stored}, nil
		},
	}

	parser := &mockFileParser{
		parseFunc: func(string, string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{
				ID:               id,
				OriginalUrl:      "https://rutracker.org/forum/viewtopic.php?t=" + id,
				Magnet:           newMagnet,
				Name:             "Test Torrent v2",
				LastComment:      "new comment",
				TorrentUpdatedAt: time.Date(2026, 3, 22, 12, 59, 0, 0, time.UTC),
			}, nil
		},
	}

	dClient := &mockDownloadClient{
		createDownloadTaskFunc: func(string, string) error { return s.downloadErr },
	}

	notifier := &recordingNotifier{}
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         dClient,
		Store:           store,
		Notifier:        notifier,
		DryMode:         s.dryMode,
	})

	return releaseRun{
		client:   client,
		notifier: notifier,
		saved:    func() *tracker.FileMetadata { return saved },
		stored:   stored,
	}
}

func TestPublishReleaseUpdate_MagnetUnchanged_NoEvent(t *testing.T) {
	run := newReleaseRun(releaseScenario{storedNotify: true, newMagnet: releaseOldMagnet})

	run.client.processFileMetadata(context.Background(), run.stored, true)

	assert.Empty(t, run.notifier.messages)
}

func TestPublishReleaseUpdate_NotifyOff_NoEvent(t *testing.T) {
	run := newReleaseRun(releaseScenario{})

	run.client.processFileMetadata(context.Background(), run.stored, true)

	assert.Empty(t, run.notifier.messages)
}

func TestPublishReleaseUpdate_FromCron_PublishesOnce(t *testing.T) {
	run := newReleaseRun(releaseScenario{storedNotify: true, location: "/downloads/tv shows"})

	run.client.processFileMetadata(context.Background(), run.stored, true)

	require.Len(t, run.notifier.messages, 1)
	msg := run.notifier.messages[0]
	assert.Equal(t, "tuclaw.releases.updated.3304959", msg.Subject)

	digest := sha256.Sum256([]byte(releaseNewMagnet))
	assert.Equal(t, "3304959:"+hex.EncodeToString(digest[:]), msg.MsgID)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(msg.Payload, &payload))
	assert.Equal(t, "3304959", payload["file_id"])
	assert.Equal(t, "Test Torrent v2", payload["name"])
	assert.Equal(t, "https://rutracker.org/forum/viewtopic.php?t=3304959", payload["page_url"])
	assert.Equal(t, "new comment", payload["last_comment"])
	assert.Equal(t, "/downloads/tv shows", payload["location"])
	assert.Equal(t, "2026-03-22T12:59:00Z", payload["torrent_updated_at"])

	updatedAt, err := time.Parse(time.RFC3339, payload["updated_at"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), updatedAt, time.Minute)
}

func TestPublishReleaseUpdate_ManualRefresh_NoEvent(t *testing.T) {
	t.Run("refresh all", func(t *testing.T) {
		run := newReleaseRun(releaseScenario{storedNotify: true})

		run.client.RefreshAll(context.Background())

		assert.Empty(t, run.notifier.messages)
	})

	t.Run("single file refresh", func(t *testing.T) {
		run := newReleaseRun(releaseScenario{storedNotify: true})

		run.client.CheckFileForUpdates(context.Background(), "3304959")

		assert.Empty(t, run.notifier.messages)
	})
}

func TestPublishReleaseUpdate_DryMode_NoEvent(t *testing.T) {
	run := newReleaseRun(releaseScenario{storedNotify: true, dryMode: true})

	run.client.processFileMetadata(context.Background(), run.stored, true)

	assert.Empty(t, run.notifier.messages)
}

func TestPublishReleaseUpdate_DownloadFailed_NoEvent(t *testing.T) {
	run := newReleaseRun(releaseScenario{storedNotify: true, downloadErr: fmt.Errorf("qbittorrent is down")})

	run.client.processFileMetadata(context.Background(), run.stored, true)

	assert.Empty(t, run.notifier.messages)
	require.NotNil(t, run.saved())
	assert.Equal(t, releaseOldMagnet, run.saved().Magnet, "the revert must leave the stored magnet alone")
}

func TestPublishReleaseUpdate_InvalidSubjectToken_LogsAndSkips(t *testing.T) {
	logs := captureLogs(t)
	run := newReleaseRun(releaseScenario{storedNotify: true, fileID: "3304959.2"})

	run.client.processFileMetadata(context.Background(), run.stored, true)

	assert.Empty(t, run.notifier.messages)

	found := false
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var entry map[string]any
		require.NoError(t, json.Unmarshal(line, &entry))
		if entry["msg"] == "file id is not a valid subject token, release update skipped" {
			found = true
			assert.Equal(t, "3304959.2", entry["id"])
			assert.Equal(t, "ERROR", entry["level"])
		}
	}
	assert.True(t, found, "an invalid subject token must be logged as an error")
}

func TestPublishReleaseUpdate_PublishError_DoesNotAbortSweep(t *testing.T) {
	run := newReleaseRun(releaseScenario{storedNotify: true})
	run.notifier.err = fmt.Errorf("nats is unreachable")

	run.client.processFileMetadata(context.Background(), run.stored, true)

	require.Len(t, run.notifier.messages, 1)
	require.NotNil(t, run.saved())
	assert.Equal(t, releaseNewMagnet, run.saved().Magnet, "a publish failure must not revert the re-download")
}

func TestPublishReleaseUpdate_NilNotifier_NoPanic(t *testing.T) {
	run := newReleaseRun(releaseScenario{storedNotify: true})
	run.client.notifier = nil

	assert.NotPanics(t, func() {
		run.client.processFileMetadata(context.Background(), run.stored, true)
	})
}

func TestProcessFileMetadata_EmptyLocation_KeepsNotify(t *testing.T) {
	run := newReleaseRun(releaseScenario{storedNotify: true})

	run.client.processFileMetadata(context.Background(), run.stored, true)

	require.NotNil(t, run.saved())
	assert.True(t, run.saved().Notify, "an empty location must not clear the notify flag")
}

func TestCreateFromURL_CarriesNotifyFlag(t *testing.T) {
	for _, notifyFlag := range []bool{true, false} {
		t.Run(fmt.Sprintf("notify=%v", notifyFlag), func(t *testing.T) {
			var saved *tracker.FileMetadata
			store := &mockFileStore{
				getByIdFunc: func(string) (*tracker.FileMetadata, error) { return nil, sql.ErrNoRows },
				createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
					saved = metadata
					return nil
				},
			}
			parser := &mockFileParser{
				parseFunc: func(string, string) (*tracker.FileMetadata, error) {
					return &tracker.FileMetadata{ID: "1", Magnet: releaseNewMagnet}, nil
				},
			}
			client := NewClient(&ClientCtx{
				MessagesForSend: make(chan string, 10),
				Tracker:         parser,
				DClient:         &mockDownloadClient{createDownloadTaskFunc: func(string, string) error { return nil }},
				Store:           store,
			})

			metadata, err := client.CreateFromURL(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1", "", notifyFlag)

			require.NoError(t, err)
			assert.Equal(t, notifyFlag, metadata.Notify)
			require.NotNil(t, saved)
			assert.Equal(t, notifyFlag, saved.Notify)
		})
	}
}

func TestCreateFromURL_ActiveRowIsRefused(t *testing.T) {
	existing := &tracker.FileMetadata{ID: "1", Magnet: releaseOldMagnet, Notify: true}

	var saved *tracker.FileMetadata
	store := &mockFileStore{
		getByIdFunc: func(string) (*tracker.FileMetadata, error) { return existing, nil },
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			saved = metadata
			return nil
		},
	}
	parser := &mockFileParser{
		parseFunc: func(string, string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "1", Magnet: releaseNewMagnet}, nil
		},
	}
	downloads := 0
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient: &mockDownloadClient{createDownloadTaskFunc: func(string, string) error {
			downloads++
			return nil
		}},
		Store: store,
	})

	_, err := client.CreateFromURL(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1", "", false)

	require.ErrorIs(t, err, types.ErrFileAlreadyTracked)
	assert.Nil(t, saved, "a refused create writes nothing")
	assert.Equal(t, 0, downloads, "a refused create never reaches qbittorrent")
}

func TestCreateFromURL_SoftDeletedRowIsRecreatedWithRequestFlag(t *testing.T) {
	existing := &tracker.FileMetadata{
		ID:       "1",
		Magnet:   releaseOldMagnet,
		Notify:   true,
		DeleteAt: sql.NullTime{Time: time.Now(), Valid: true},
	}

	var saved *tracker.FileMetadata
	store := &mockFileStore{
		getByIdFunc: func(string) (*tracker.FileMetadata, error) { return existing, nil },
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			saved = metadata
			return nil
		},
	}
	parser := &mockFileParser{
		parseFunc: func(string, string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "1", Magnet: releaseNewMagnet}, nil
		},
	}
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{createDownloadTaskFunc: func(string, string) error { return nil }},
		Store:           store,
	})

	_, err := client.CreateFromURL(context.Background(), "https://rutracker.org/forum/viewtopic.php?t=1", "", false)

	require.NoError(t, err)
	require.NotNil(t, saved)
	assert.False(t, saved.Notify, "a re-create takes the request's flag, it does not inherit the dead row's")
}

func TestUpdateTaskSettings(t *testing.T) {
	on := true
	movies := "/downloads/movies"

	tests := []struct {
		name         string
		notify       *bool
		location     *string
		wantNotify   bool
		wantLocation string
	}{
		{name: "notify only", notify: &on, wantNotify: true, wantLocation: "/downloads/cinema-prep"},
		{name: "location only", location: &movies, wantNotify: false, wantLocation: "/downloads/movies"},
		{name: "both", notify: &on, location: &movies, wantNotify: true, wantLocation: "/downloads/movies"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotNotify bool
			var gotLocation string
			store := &mockFileStore{
				getByIdFunc: func(string) (*tracker.FileMetadata, error) {
					return &tracker.FileMetadata{ID: "1", Location: "/downloads/cinema-prep"}, nil
				},
				updateSettingsFunc: func(_ string, notify bool, location string) error {
					gotNotify, gotLocation = notify, location
					return nil
				},
			}
			client := NewClient(&ClientCtx{MessagesForSend: make(chan string, 10), Store: store})

			file, err := client.UpdateTaskSettings("1", tt.notify, tt.location)

			require.NoError(t, err)
			assert.Equal(t, tt.wantNotify, gotNotify)
			assert.Equal(t, tt.wantLocation, gotLocation)
			assert.Equal(t, tt.wantNotify, file.Notify)
			assert.Equal(t, tt.wantLocation, file.Location)
		})
	}
}

func TestUpdateTaskSettings_DeletedOrMissingIsNotFound(t *testing.T) {
	on := true
	updates := 0
	store := &mockFileStore{
		getByIdFunc: func(id string) (*tracker.FileMetadata, error) {
			if id == "missing" {
				return nil, sql.ErrNoRows
			}
			return &tracker.FileMetadata{ID: id, DeleteAt: sql.NullTime{Time: time.Now(), Valid: true}}, nil
		},
		updateSettingsFunc: func(string, bool, string) error {
			updates++
			return nil
		},
	}
	client := NewClient(&ClientCtx{MessagesForSend: make(chan string, 10), Store: store})

	_, err := client.UpdateTaskSettings("missing", &on, nil)
	require.ErrorIs(t, err, sql.ErrNoRows)

	_, err = client.UpdateTaskSettings("deleted", &on, nil)
	require.ErrorIs(t, err, sql.ErrNoRows)

	assert.Equal(t, 0, updates, "a dead or missing task is never written")
}

func TestOnMessage_NeverArmsTheAgent(t *testing.T) {
	var saved *tracker.FileMetadata
	store := &mockFileStore{
		getByIdFunc: func(string) (*tracker.FileMetadata, error) { return nil, sql.ErrNoRows },
		createOrReplaceFunc: func(metadata *tracker.FileMetadata) error {
			saved = metadata
			return nil
		},
	}
	parser := &mockFileParser{
		parseFunc: func(string, string) (*tracker.FileMetadata, error) {
			return &tracker.FileMetadata{ID: "1", Magnet: releaseNewMagnet, Notify: true}, nil
		},
	}
	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 10),
		Tracker:         parser,
		DClient:         &mockDownloadClient{createDownloadTaskFunc: func(string, string) error { return nil }},
		Store:           store,
	})

	ok, _, err := client.OnMessage(context.Background(), bot.Message{Text: "https://rutracker.org/forum/viewtopic.php?t=1"}, "")

	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, saved)
	assert.False(t, saved.Notify)
}
