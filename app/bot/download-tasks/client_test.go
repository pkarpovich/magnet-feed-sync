package download_tasks

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"magnet-feed-sync/app/tracker"
	"magnet-feed-sync/app/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockFileParser struct {
	parseFunc func(url, location string) (*tracker.FileMetadata, error)
}

func (m *mockFileParser) Parse(url, location string) (*tracker.FileMetadata, error) {
	return m.parseFunc(url, location)
}

type mockFileStore struct {
	getByIdFunc         func(id string) (*tracker.FileMetadata, error)
	createOrReplaceFunc func(metadata *tracker.FileMetadata) error
	getAllFunc           func() ([]*tracker.FileMetadata, error)
	removeFunc          func(id string) error
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

type mockDownloadClient struct {
	createDownloadTaskFunc func(url, destination string) error
}

func (m *mockDownloadClient) CreateDownloadTask(url, destination string) error {
	return m.createDownloadTaskFunc(url, destination)
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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      magnet,
	})

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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      oldMagnet,
	})

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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      magnet,
	})

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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	})

	assert.Empty(t, msgChan, "no notification should be sent on parse error")
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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "",
	})
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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	})

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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      oldMagnet,
	})

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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
		Magnet:      oldMagnet,
	})

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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "3304959",
		OriginalUrl: "https://rutracker.org/forum/viewtopic.php?t=3304959",
	})

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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "test-v2",
		OriginalUrl: "https://example.com/topic/123",
	})

	assert.True(t, downloadCalled, "download should trigger when magnets differ and have no btih hash")
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
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
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

	client.processFileMetadata(&tracker.FileMetadata{
		ID:          "123",
		OriginalUrl: "https://example.com/topic/123",
	})

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

	client.CheckForUpdates()

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

	client.CheckForUpdates()
}
