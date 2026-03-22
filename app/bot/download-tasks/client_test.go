package download_tasks

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

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

func TestClient_InterfaceSatisfaction(t *testing.T) {
	parser := &mockFileParser{}
	store := &mockFileStore{}

	var _ FileParser = parser
	var _ FileStore = store

	client := NewClient(&ClientCtx{
		MessagesForSend: make(chan string, 1),
		Tracker:         parser,
		Store:           store,
		DryMode:         true,
	})

	assert.NotNil(t, client)
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
