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
		MiniAppUrl:      "https://40643a68b9e5.ngrok-free.app",
		MiniAppUrlAdmin: "https://40643a68b9e5.ngrok-free.app/admin-show-catalog",
		AdminID:         800703982,
	}, nil
}
