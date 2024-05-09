package events

import (
	"encoding/json"
	"errors"
	"fmt"
	tbapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"log"
	"magnet-feed-sync/app/bot"
	downloadTask "magnet-feed-sync/app/bot/download-tasks"
	taskStore "magnet-feed-sync/app/task-store"
)

const (
	PingCommand           = "ping"
	GetActiveTasksCommand = "get_active_tasks"
	RemoveTaskCallback    = "remove_task"
)

type Bot interface {
	OnMessage(msg bot.Message) (bool, string, error)
}

type TbAPI interface {
	GetUpdatesChan(config tbapi.UpdateConfig) tbapi.UpdatesChannel
	Send(c tbapi.Chattable) (tbapi.Message, error)
	Request(c tbapi.Chattable) (*tbapi.APIResponse, error)
}

type TelegramListener struct {
	SuperUsers      []int64
	TbAPI           TbAPI
	Bot             Bot
	Store           *taskStore.Repository
	MessagesForSend chan string
}

type RemoveTaskData struct {
	TaskID string `json:"taskId"`
	Type   string `json:"type"`
}

func (tl *TelegramListener) Do() error {
	u := tbapi.NewUpdate(0)
	u.Timeout = 60

	updates := tl.TbAPI.GetUpdatesChan(u)

	for {
		select {

		case update, ok := <-updates:
			if !ok {
				return fmt.Errorf("telegram update chan closed")
			}

			if update.CallbackQuery != nil {
				if err := tl.processCallbackQuery(update); err != nil {
					log.Printf("[ERROR] %v", err)
				}

				continue
			}

			if update.Message == nil {
				continue
			}

			if err := tl.processEvent(update); err != nil {
				log.Printf("[ERROR] %v", err)
			}
		}
	}
}

func (tl *TelegramListener) processEvent(update tbapi.Update) error {
	msgJSON, errJSON := json.Marshal(update.Message)
	if errJSON != nil {
		return fmt.Errorf("failed to marshal update.Message to json: %w", errJSON)
	}
	log.Printf("[DEBUG] %s", string(msgJSON))

	if !tl.isSuperUser(update.Message.From.ID) {
		log.Printf("[DEBUG] user %d is not super user", update.Message.From.ID)

		msg := tbapi.NewMessage(update.Message.Chat.ID, "I don't know you 🤷‍")
		_, err := tl.TbAPI.Send(msg)
		if err != nil {
			return fmt.Errorf("failed to send message: %w", err)
		}

		return nil
	}

	switch update.Message.Command() {
	case PingCommand:
		tl.handlePingCommand(update)
		return nil

	case GetActiveTasksCommand:
		tl.handleGetActiveTasksCommand(update)
		return nil
	}

	msg := tl.transform(update.Message)
	saved, replyMsg, err := tl.Bot.OnMessage(msg)
	if err != nil {
		errMsg := tbapi.NewMessage(update.Message.Chat.ID, "💥 Error: "+err.Error())
		_, err := tl.TbAPI.Send(errMsg)
		if err != nil {
			return fmt.Errorf("failed to send error message: %w", err)
		}

		return errors.New(errMsg.Text)
	}

	if !saved {
		return nil
	}

	if err := tl.reactToMessage(update.Message.Chat.ID, update.Message.MessageID, tbapi.ReactionType{
		Type:  "emoji",
		Emoji: "👍",
	}); err != nil {
		return fmt.Errorf("failed to react to message: %w", err)
	}

	if len(replyMsg) > 0 {
		if _, err := tl.TbAPI.Send(NewMarkdownMessage(update.Message.Chat.ID, replyMsg, nil)); err != nil {
			return fmt.Errorf("failed to reply send message: %w", err)
		}
	}

	return nil
}

func (tl *TelegramListener) processCallbackQuery(update tbapi.Update) error {
	rawMsgData := update.CallbackQuery.Data
	var data RemoveTaskData

	if err := json.Unmarshal([]byte(rawMsgData), &data); err != nil {
		return fmt.Errorf("failed to unmarshal callback data: %w", err)
	}

	switch data.Type {
	case RemoveTaskCallback:
		if err := tl.Store.Remove(data.TaskID); err != nil {
			errMsg := tbapi.NewMessage(update.CallbackQuery.Message.Chat.ID, "💥 Error: "+err.Error())
			_, err := tl.TbAPI.Send(errMsg)
			if err != nil {
				return fmt.Errorf("failed to send error message: %w", err)
			}

			return errors.New(errMsg.Text)
		}

		msg := tbapi.NewDeleteMessage(update.CallbackQuery.Message.Chat.ID, update.CallbackQuery.Message.MessageID)
		_, err := tl.TbAPI.Send(msg)
		if err != nil {
			return fmt.Errorf("failed to delete message: %w", err)
		}
	}

	return nil
}

func (tl *TelegramListener) transform(message *tbapi.Message) bot.Message {
	msg := bot.Message{
		ID:     message.MessageID,
		From:   bot.User{},
		ChatID: message.Chat.ID,
		HTML:   message.Text,
		Text:   message.Text,
		Sent:   message.Time(),
	}

	if len(message.Caption) > 0 {
		msg.Text = message.Caption
	}

	if message.ForwardOrigin != nil {
		origin := message.ForwardOrigin

		switch message.ForwardOrigin.Type {
		case tbapi.MessageOriginChannel:
			msg.Url = fmt.Sprintf("https://t.me/%s/%d", origin.Chat.UserName, origin.MessageID)
		case tbapi.MessageOriginUser:
			msg.Text = fmt.Sprintf(
				"%s %s (%s):\n%s",
				origin.SenderUser.FirstName,
				origin.SenderUser.LastName,
				origin.SenderUser.UserName,
				message.Text,
			)
		case tbapi.MessageOriginHiddenUser:
			msg.Text = fmt.Sprintf("%s:\n%s", origin.SenderUserName, message.Text)
		}
	}

	return msg
}

func (tl *TelegramListener) handlePingCommand(update tbapi.Update) {
	msg := tbapi.NewMessage(update.Message.Chat.ID, "🏓 Pong!")
	_, err := tl.TbAPI.Send(msg)
	if err != nil {
		log.Printf("[ERROR] failed to send message: %v", err)
	}
}

func (tl *TelegramListener) handleGetActiveTasksCommand(update tbapi.Update) {
	tasks, err := tl.Store.GetAll()
	if err != nil {
		errMsg := tbapi.NewMessage(update.Message.Chat.ID, "💥 Error: "+err.Error())
		_, err := tl.TbAPI.Send(errMsg)
		if err != nil {
			log.Printf("[ERROR] failed to send error message: %v", err)
		}

		return
	}

	if err := tl.reactToMessage(update.Message.Chat.ID, update.Message.MessageID, tbapi.ReactionType{
		Type:  "emoji",
		Emoji: "👍",
	}); err != nil {
		log.Printf("[ERROR] failed to react to message: %v", err)

		return
	}

	if len(tasks) == 0 {
		msg := tbapi.NewMessage(update.Message.Chat.ID, "📭 No active tasks")
		_, err := tl.TbAPI.Send(msg)
		if err != nil {
			log.Printf("[ERROR] failed to send message: %v", err)
		}

		return
	}

	for _, task := range tasks {
		replyMsg, err := downloadTask.MetadataToMsg(task)
		if err != nil {
			log.Printf("[ERROR] failed to format metadata: %v", err)
			continue
		}

		replyMarkup, err := buildReplyMarkup([]ReplyMarkupButton{
			{
				Text: "❌",
				Data: map[string]interface{}{
					"type":   RemoveTaskCallback,
					"taskId": task.ID,
				},
			},
		})
		if err != nil {
			log.Printf("[ERROR] failed to build reply markup: %v", err)
		}

		msg := NewMarkdownMessage(update.Message.Chat.ID, replyMsg, &replyMarkup)
		_, err = tl.TbAPI.Send(msg)
		if err != nil {
			log.Printf("[ERROR] failed to send message: %v", err)
		}
	}
}

func (tl *TelegramListener) SendMessagesForAdmins() {
	adminIds := tl.SuperUsers

	for {
		select {
		case msg := <-tl.MessagesForSend:
			for _, adminID := range adminIds {
				_, err := tl.TbAPI.Send(NewMarkdownMessage(adminID, msg, nil))
				if err != nil {
					log.Printf("[ERROR] failed to send message: %v", err)
				}
			}
		}
	}
}

func (tl *TelegramListener) isSuperUser(userID int64) bool {
	for _, su := range tl.SuperUsers {
		if su == userID {
			return true
		}
	}

	return false
}

func (tl *TelegramListener) reactToMessage(chatID int64, messageID int, reaction tbapi.ReactionType) error {
	reactionMsg := tbapi.SetMessageReactionConfig{
		BaseChatMessage: tbapi.BaseChatMessage{
			ChatConfig: tbapi.ChatConfig{
				ChatID: chatID,
			},
			MessageID: messageID,
		},
		Reaction: []tbapi.ReactionType{reaction},
		IsBig:    false,
	}

	_, err := tl.TbAPI.Request(reactionMsg)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

func NewMarkdownMessage(chatID int64, text string, replyMarkup *tbapi.InlineKeyboardMarkup) tbapi.MessageConfig {
	return tbapi.MessageConfig{
		BaseChat: tbapi.BaseChat{
			ChatConfig: tbapi.ChatConfig{
				ChatID: chatID,
			},
			ReplyMarkup: replyMarkup,
		},
		LinkPreviewOptions: tbapi.LinkPreviewOptions{
			IsDisabled: false,
		},
		ParseMode: tbapi.ModeMarkdownV2,
		Text:      text,
	}
}

func NewMessage(chatID int64, text string) tbapi.MessageConfig {
	return tbapi.NewMessage(chatID, text)
}
