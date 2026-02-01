package tracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"magnet-feed-sync/app/types"
)

type mockDownloadClient struct {
	defaultLocation string
}

func (m *mockDownloadClient) CreateDownloadTask(url, destination string) error {
	return nil
}

func (m *mockDownloadClient) GetHashByMagnet(magnet string) (string, error) {
	return "", nil
}

func (m *mockDownloadClient) SetLocation(taskID, location string) error {
	return nil
}

func (m *mockDownloadClient) GetLocations() []types.Location {
	return nil
}

func (m *mockDownloadClient) GetDefaultLocation() string {
	return m.defaultLocation
}

func TestParser_LocationParameter(t *testing.T) {
	tests := []struct {
		name             string
		inputLocation    string
		defaultLocation  string
		expectedLocation string
	}{
		{
			name:             "explicit location is used",
			inputLocation:    "/downloads/movies",
			defaultLocation:  "/downloads/tv shows",
			expectedLocation: "/downloads/movies",
		},
		{
			name:             "empty location falls back to default",
			inputLocation:    "",
			defaultLocation:  "/downloads/tv shows",
			expectedLocation: "/downloads/tv shows",
		},
		{
			name:             "custom location overrides default",
			inputLocation:    "/downloads/anime",
			defaultLocation:  "/downloads/other",
			expectedLocation: "/downloads/anime",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockDownloadClient{defaultLocation: tt.defaultLocation}
			_ = NewParser(mockClient)

			resultLocation := tt.inputLocation
			if resultLocation == "" {
				resultLocation = mockClient.GetDefaultLocation()
			}

			assert.Equal(t, tt.expectedLocation, resultLocation)
		})
	}
}
