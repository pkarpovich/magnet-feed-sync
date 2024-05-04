module magnet-feed-sync

go 1.22

require (
	github.com/PuerkitoBio/goquery v1.9.2
	github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1
	github.com/ilyakaznacheev/cleanenv v1.5.0
	github.com/joho/godotenv v1.5.1
	github.com/mmcdole/gofeed v1.3.0
	golang.org/x/net v0.24.0
)

replace github.com/go-telegram-bot-api/telegram-bot-api/v5 v5.5.1 => github.com/OvyFlash/telegram-bot-api/v5 v5.0.0-20240427121735-f3a5b4ed79f6

require (
	github.com/BurntSushi/toml v1.3.2 // indirect
	github.com/andybalholm/cascadia v1.3.2 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mmcdole/goxpp v1.1.1-0.20240225020742-a0c311522b23 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/stretchr/testify v1.8.4 // indirect
	golang.org/x/text v0.15.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	olympos.io/encoding/edn v0.0.0-20201019073823-d3554ca0b0a3 // indirect
)
