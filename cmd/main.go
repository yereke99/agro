// cmd/main.go
package main

import (
	"agro/config"
	"agro/internal/handler"
	"agro/internal/repository"
	"agro/traits/database"
	"agro/traits/logger"
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-telegram/bot"
	"go.uber.org/zap"
)

func main() {
	zapLogger, err := logger.NewLogger()
	if err != nil {
		panic(err)
	}

	cfg, err := config.NewConfig()
	if err != nil {
		zapLogger.Error("error init config", zap.Error(err))
		return
	}

	db, err := database.InitDatabase(cfg.DBPath)
	if err != nil {
		zapLogger.Error("error initializing database", zap.Error(err))
		return
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())

	redisClient, err := database.ConnectRedis(ctx, zapLogger)
	if err != nil {
		zapLogger.Fatal("error conn to redis", zap.Error(err))
	}
	redisRepo := repository.NewRedisClient(redisClient)

	handl := handler.NewHandler(zapLogger, cfg, ctx, db, redisRepo)

	opts := []bot.Option{
		// Разрешаем сообщения и callback_query
		bot.WithAllowedUpdates([]string{"message", "callback_query"}),

		// Админ-команды
		bot.WithMessageTextHandler("/admin", bot.MatchTypeExact, handl.AdminHandler),
		bot.WithMessageTextHandler("📢 Хабарлама (Messages)", bot.MatchTypeExact, handl.AdminHandler),
		bot.WithMessageTextHandler("❌ Жабу (Close)", bot.MatchTypeExact, handl.AdminHandler),

		// ✅ Хендлер для inline-кнопок оплаты ЗАКАЗОВ (pay_ok:... / pay_reject:...)
		bot.WithCallbackQueryDataHandler("pay_", bot.MatchTypePrefix, handl.PaymentCallbackHandler),

		// ✅ Хендлер для inline-кнопок оплаты ПОДПИСОК (sub_ok:... / sub_reject:...)
		bot.WithCallbackQueryDataHandler("sub_", bot.MatchTypePrefix, handl.PaymentCallbackHandler),

		// Дефолтный хендлер (приветствие + мини-апп)
		bot.WithDefaultHandler(handl.DefaultHandler),
	}

	b, err := bot.New(cfg.Token, opts...)
	if err != nil {
		zapLogger.Error("error in start bot", zap.Error(err))
		return
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGINT)

	go func() {
		<-stop
		zapLogger.Info("Bot stopped successfully")
		cancel()
	}()

	go handl.StartWebServer(ctx, b)
	zapLogger.Info("Starting web server", zap.String("port", cfg.Port))
	zapLogger.Info("Bot started successfully")

	b.Start(ctx)
}
