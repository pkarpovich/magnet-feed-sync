package utils

import (
	"fmt"
	"strings"
	"time"
)

func ParseRussianDate(dateStr string) (time.Time, error) {
	russianMonths := map[string]string{
		"Янв": "Jan", "Фев": "Feb", "Мар": "Mar", "Апр": "Apr",
		"Май": "May", "Июн": "Jun", "Июл": "Jul", "Авг": "Aug",
		"Сен": "Sep", "Окт": "Oct", "Ноя": "Nov", "Дек": "Dec",
	}

	var layout string
	if strings.Contains(dateStr, "-") {
		layout = "02 Jan 06 15:04"
	} else {
		layout = "02 Jan 2006 15:04:05"
	}

	parts := strings.FieldsFunc(dateStr, func(r rune) bool {
		return r == ' ' || r == '-'
	})

	if len(parts) < 4 {
		return time.Time{}, fmt.Errorf("incorrect date format")
	}

	month, ok := russianMonths[parts[1]]
	if !ok {
		return time.Time{}, fmt.Errorf("invalid month")
	}
	parts[1] = month

	normalizedDateStr := strings.Join(parts, " ")

	parsedDate, err := time.Parse(layout, normalizedDateStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("could not parse date: %v", err)
	}

	return parsedDate, nil
}
