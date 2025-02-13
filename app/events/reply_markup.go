package events

import (
	"encoding/json"
	tbapi "github.com/OvyFlash/telegram-bot-api"
)

type ReplyMarkupButton struct {
	Text string
	Data map[string]interface{}
}

func buildReplyMarkup(buttons []ReplyMarkupButton) (tbapi.InlineKeyboardMarkup, error) {
	markup := tbapi.NewInlineKeyboardMarkup()
	baseRow := tbapi.NewInlineKeyboardRow()

	for _, button := range buttons {
		jsonData, err := packButtonData(button.Data)
		if err != nil {
			return markup, err
		}

		baseRow = append(baseRow, tbapi.NewInlineKeyboardButtonData(button.Text, jsonData))
	}

	markup.InlineKeyboard = append(markup.InlineKeyboard, baseRow)

	return markup, nil
}

func packButtonData(data interface{}) (string, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return "", err
	}

	return string(jsonData), nil
}
