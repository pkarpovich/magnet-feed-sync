package config

import (
	"github.com/ilyakaznacheev/cleanenv"
	"github.com/joho/godotenv"
	"log"
)

type SynologyConfig struct {
	URL      string `env:"SYNOLOGY_URL" env-default:"http://localhost:5000"`
	Username string `env:"SYNOLOGY_USERNAME"`
	Password string `env:"SYNOLOGY_PASSWORD"`
}

type Config struct {
	Synology SynologyConfig
}

func Init() (*Config, error) {
	err := godotenv.Load()
	if err != nil {
		log.Printf("[WARN] error while loading .env file: %v", err)
	}

	var cfg Config
	err = cleanenv.ReadEnv(&cfg)
	if err != nil {
		return nil, err
	}

	return &cfg, nil
}
