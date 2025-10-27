package config

import (
	"os"
)

type Config struct {
	Token           string
	Port            string
	DBPath          string
	ChannelName     string
	MiniAppUrl      string
	MiniAppUrlAdmin string
	AdminID         int64
	YandexAPIKey    string
}

func NewConfig() (*Config, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		token = "8288790284:AAHkDouevMu_7ddQk9CleHDrOdRqFalBV-M"
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./agro.db"
	}

	return &Config{
		Token:           token,
		Port:            port,
		DBPath:          dbPath,
		ChannelName:     "@jaiAngmeAitamyz",
		MiniAppUrl:      "https://52f577865a02.ngrok-free.app",
		MiniAppUrlAdmin: "https://52f577865a02.ngrok-free.app/admin-show-catalog",
		YandexAPIKey:    "8a3e4da0-9ef2-4176-9203-e7014c1dba6f",
		AdminID:         800703982,
	}, nil
}
