package download_tasks

import (
	"testing"

	"magnet-feed-sync/app/tracker"

	"github.com/stretchr/testify/assert"
)

type mockFileParser struct {
	parseFunc func(url, location string) (*tracker.FileMetadata, error)
}

func (m *mockFileParser) Parse(url, location string) (*tracker.FileMetadata, error) {
	return m.parseFunc(url, location)
}

type mockFileStore struct {
	getByIdFunc        func(id string) (*tracker.FileMetadata, error)
	createOrReplaceFunc func(metadata *tracker.FileMetadata) error
	getAllFunc          func() ([]*tracker.FileMetadata, error)
	removeFunc         func(id string) error
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
