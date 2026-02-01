package events

import (
	"testing"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"magnet-feed-sync/app/bot"
)

type mockBot struct {
	lastMessage  bot.Message
	lastLocation string
	returnSaved  bool
	returnReply  string
	returnError  error
}

func (m *mockBot) OnMessage(msg bot.Message, location string) (bool, string, error) {
	m.lastMessage = msg
	m.lastLocation = location
	return m.returnSaved, m.returnReply, m.returnError
}

type mockTbAPI struct {
	sentMessages []tbapi.Chattable
}

func (m *mockTbAPI) GetUpdatesChan(config tbapi.UpdateConfig) tbapi.UpdatesChannel {
	return make(tbapi.UpdatesChannel)
}

func (m *mockTbAPI) Send(c tbapi.Chattable) (tbapi.Message, error) {
	m.sentMessages = append(m.sentMessages, c)
	return tbapi.Message{}, nil
}

func (m *mockTbAPI) Request(c tbapi.Chattable) (*tbapi.APIResponse, error) {
	return &tbapi.APIResponse{Ok: true}, nil
}

func TestFolderCommands(t *testing.T) {
	tests := []struct {
		name             string
		command          string
		expectedLocation string
	}{
		{
			name:             "movies command",
			command:          "movies",
			expectedLocation: "/downloads/movies",
		},
		{
			name:             "books command",
			command:          "books",
			expectedLocation: "/downloads/books",
		},
		{
			name:             "music command",
			command:          "music",
			expectedLocation: "/downloads/music",
		},
		{
			name:             "other command",
			command:          "other",
			expectedLocation: "/downloads/other",
		},
		{
			name:             "comics command",
			command:          "comics",
			expectedLocation: "/downloads/comics",
		},
		{
			name:             "podcasts command",
			command:          "podcasts",
			expectedLocation: "/downloads/podcasts",
		},
		{
			name:             "audiobooks command",
			command:          "audiobooks",
			expectedLocation: "/downloads/audiobooks",
		},
		{
			name:             "anime command",
			command:          "anime",
			expectedLocation: "/downloads/anime",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			location, ok := folderCommands[tt.command]
			require.True(t, ok, "command %s should exist in folderCommands", tt.command)
			assert.Equal(t, tt.expectedLocation, location)
		})
	}
}

func TestProcessEvent_FolderCommands(t *testing.T) {
	tests := []struct {
		name             string
		messageText      string
		commandLength    int
		expectedURL      string
		expectedLocation string
	}{
		{
			name:             "movies command with URL",
			messageText:      "/movies https://rutracker.org/forum/viewtopic.php?t=123",
			commandLength:    7,
			expectedURL:      "https://rutracker.org/forum/viewtopic.php?t=123",
			expectedLocation: "/downloads/movies",
		},
		{
			name:             "books command with URL",
			messageText:      "/books https://rutracker.org/forum/viewtopic.php?t=456",
			commandLength:    6,
			expectedURL:      "https://rutracker.org/forum/viewtopic.php?t=456",
			expectedLocation: "/downloads/books",
		},
		{
			name:             "anime command with URL",
			messageText:      "/anime https://nnmclub.to/forum/viewtopic.php?t=789",
			commandLength:    6,
			expectedURL:      "https://nnmclub.to/forum/viewtopic.php?t=789",
			expectedLocation: "/downloads/anime",
		},
		{
			name:             "plain URL without command",
			messageText:      "https://rutracker.org/forum/viewtopic.php?t=999",
			commandLength:    0,
			expectedURL:      "https://rutracker.org/forum/viewtopic.php?t=999",
			expectedLocation: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockB := &mockBot{returnSaved: true, returnReply: ""}
			mockAPI := &mockTbAPI{}

			tl := &TelegramListener{
				SuperUsers: []int64{123},
				TbAPI:      mockAPI,
				Bot:        mockB,
			}

			var entities []tbapi.MessageEntity
			if tt.commandLength > 0 {
				entities = []tbapi.MessageEntity{
					{Type: "bot_command", Offset: 0, Length: tt.commandLength},
				}
			}

			update := tbapi.Update{
				Message: &tbapi.Message{
					Text:     tt.messageText,
					Chat:     tbapi.Chat{ID: 1},
					From:     &tbapi.User{ID: 123},
					Entities: entities,
				},
			}

			err := tl.processEvent(update)
			require.NoError(t, err)

			assert.Equal(t, tt.expectedURL, mockB.lastMessage.Text)
			assert.Equal(t, tt.expectedLocation, mockB.lastLocation)
		})
	}
}

func TestProcessEvent_NonSuperUser(t *testing.T) {
	mockB := &mockBot{}
	mockAPI := &mockTbAPI{}

	tl := &TelegramListener{
		SuperUsers: []int64{123},
		TbAPI:      mockAPI,
		Bot:        mockB,
	}

	update := tbapi.Update{
		Message: &tbapi.Message{
			Text: "/movies https://example.com",
			Chat: tbapi.Chat{ID: 1},
			From: &tbapi.User{ID: 456},
			Entities: []tbapi.MessageEntity{
				{Type: "bot_command", Offset: 0, Length: 7},
			},
		},
	}

	err := tl.processEvent(update)
	require.NoError(t, err)

	assert.Empty(t, mockB.lastMessage.Text, "bot should not receive message from non-super user")
	assert.Len(t, mockAPI.sentMessages, 1, "should send rejection message")
}
