package config

import (
	"os"
	"strconv"
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
	KaspiPayURL     string

	// 🔹 Новые поля для оплаты переводом
	KaspiCardNumber string
	KaspiCardHolder string
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func NewConfig() (*Config, error) {
	token := envOrDefault("TELEGRAM_BOT_TOKEN",
		"8288790284:AAHkDouevMu_7ddQk9CleHDrOdRqFalBV-M")

	port := envOrDefault("PORT", "8081")
	dbPath := envOrDefault("DB_PATH", "./agro.db")

	miniAppUrl := "https://meily.kz"

	// Admin ID — можно переопределить через ENV ADMIN_ID
	adminIDStr := envOrDefault("ADMIN_ID", "800703982")
	adminID, _ := strconv.ParseInt(adminIDStr, 10, 64)

	// Kaspi Pay по ссылке (инвойс)
	kaspiPayURL := envOrDefault("KASPI_PAY_URL",
		"https://pay.kaspi.kz/pay/e96vsxbs")

	// 🔹 Реквизиты для перевода на Kaspi Gold (по умолчанию можно свои поставить)
	kaspiCardNumber := envOrDefault("KASPI_CARD_NUMBER",
		"4400 4300 0000 1234")
	kaspiCardHolder := envOrDefault("KASPI_CARD_HOLDER",
		"AGRO CLUB")

	return &Config{
		Token:           token,
		Port:            port,
		DBPath:          dbPath,
		ChannelName:     "@jaiAngmeAitamyz",
		MiniAppUrl:      miniAppUrl,
		MiniAppUrlAdmin: miniAppUrl + "/admin-show-catalog",
		YandexAPIKey:    "8a3e4da0-9ef2-4176-9203-e7014c1dba6f",
		KaspiPayURL:     kaspiPayURL,
		AdminID:         adminID,

		KaspiCardNumber: kaspiCardNumber,
		KaspiCardHolder: kaspiCardHolder,
	}, nil
}
