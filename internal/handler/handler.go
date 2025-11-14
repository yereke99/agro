// handler/handler.go
package handler

import (
	"agro/config"
	"agro/internal/domain"
	"agro/internal/repository"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// способы оплаты
const (
	paymentKaspiLink     = "kaspi_link"
	paymentKaspiTransfer = "kaspi_transfer"
	paymentCash          = "cash"
)

// реквизиты Kaspi Gold (можно поменять под себя)
const (
	kaspiGoldNumber    = "4400 4301 1234 5678"
	kaspiGoldOwnerName = "ИП «АГРО Клуб»"
)

type deliveryIn struct {
	Type    string  `json:"type"` // "delivery" или "pickup"
	Address string  `json:"address"`
	Phone   string  `json:"phone"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
}

type confirmOrderIn struct {
	TelegramID    json.RawMessage `json:"telegram_id"`
	Items         []orderItemIn   `json:"items"`
	Delivery      deliveryIn      `json:"delivery"`
	PaymentMethod string          `json:"payment_method"` // kaspi_link | kaspi_transfer | cash
}

type Handler struct {
	logger      *zap.Logger
	cfg         *config.Config
	bot         *bot.Bot
	ctx         context.Context
	userRepo    *repository.UserRepository
	redisClient *repository.ChatRepository
	db          *sql.DB
}

func NewHandler(logger *zap.Logger, cfg *config.Config, ctx context.Context, db *sql.DB, redisClient *repository.ChatRepository) *Handler {
	return &Handler{
		logger:      logger,
		cfg:         cfg,
		ctx:         ctx,
		userRepo:    repository.NewUserRepository(db),
		redisClient: redisClient,
		db:          db,
	}
}

func (h *Handler) SetBot(b *bot.Bot) { h.bot = b }

// ======================== TELEGRAM HANDLERS ========================

func (h *Handler) DefaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}

	// 1) Если пользователь прислал документ (PDF/скрин), а его состояние waiting_payment —
	//    считаем это подтверждением оплаты и шлём админу.
	if update.Message.Document != nil && h.redisClient != nil {
		userID := update.Message.From.ID

		state, err := h.redisClient.GetUserState(ctx, userID)
		if err != nil {
			h.logger.Warn("get user state from redis", zap.Error(err))
		}
		if state != nil && state.State == stateWaitingPayment {
			if err := h.handlePaymentDocument(ctx, b, update, state); err != nil {
				h.logger.Warn("handle payment document", zap.Error(err))
			}
			return
		}
	}

	// 2) Обычное приветствие + кнопка mini-app
	text := "👋 Привет! Добро пожаловать в «АГРО Клуб Оптовых Цен».\n" +
		"Нажмите кнопку ниже, чтобы открыть мини-приложение и увидеть оптовые цены, оформить подписку и сделать заказ."

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{
					Text:   "🚀 Открыть мини-апп",
					WebApp: &models.WebAppInfo{URL: h.cfg.MiniAppUrl},
				},
			},
		},
	}

	if update.Message.From.ID == h.cfg.AdminID {
		kb = &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					{
						Text:   "🚀 Открыть мини-апп",
						WebApp: &models.WebAppInfo{URL: h.cfg.MiniAppUrl},
					},
					{
						Text:   "🛠 Admin",
						WebApp: &models.WebAppInfo{URL: h.cfg.MiniAppUrlAdmin},
					},
				},
			},
		}
	}

	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      update.Message.Chat.ID,
		Text:        text,
		ReplyMarkup: kb,
	})
	if err != nil {
		h.logger.Error("send welcome miniapp button", zap.Error(err))
	}
}

// Хендлер callback-ов от админа по оплатам (заказы + подписки)
// Регистрация, например:
// bot.WithCallbackQueryDataHandler("pay_", bot.MatchTypePrefix, handl.PaymentCallbackHandler),
// bot.WithCallbackQueryDataHandler("sub_", bot.MatchTypePrefix, handl.PaymentCallbackHandler),
func (h *Handler) PaymentCallbackHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery == nil {
		return
	}

	data := strings.TrimSpace(update.CallbackQuery.Data)
	if data == "" {
		return
	}

	var action string
	switch {
	case strings.HasPrefix(data, "pay_ok:"):
		action = "pay_ok"
	case strings.HasPrefix(data, "pay_reject:"):
		action = "pay_reject"
	case strings.HasPrefix(data, "sub_ok:"):
		action = "sub_ok"
	case strings.HasPrefix(data, "sub_reject:"):
		action = "sub_reject"
	default:
		return
	}

	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return
	}
	idStr := parts[1] // orderID или subscriptionID
	userIDStr := parts[2]

	mainID, _ := strconv.ParseInt(idStr, 10, 64)
	userID, _ := strconv.ParseInt(userIDStr, 10, 64)

	switch action {
	// --------- Подтверждение оплаты заказа ----------
	case "pay_ok":
		// отмечаем заказ как оплаченный
		if mainID > 0 {
			_, err := h.db.Exec(`UPDATE orders SET status = 'paid', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, mainID)
			if err != nil {
				h.logger.Error("update order status paid", zap.Error(err))
			}
		}

		// обновляем состояние пользователя
		if h.redisClient != nil && userID != 0 {
			state, err := h.redisClient.GetUserState(ctx, userID)
			if err != nil {
				h.logger.Warn("get user state for update", zap.Error(err))
			}
			if state == nil {
				state = &domain.UserState{}
			}
			state.State = stateStart
			state.IsPaid = true
			if err := h.redisClient.SaveUserState(ctx, userID, state); err != nil {
				h.logger.Warn("save user state after paid", zap.Error(err))
			}
		}

		// ответ на callback админу
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: update.CallbackQuery.ID,
			Text:            "Оплата заказа подтверждена ✅",
			ShowAlert:       false,
		})

		// уведомляем пользователя
		if userID != 0 {
			text := fmt.Sprintf("✅ Ваша оплата по заказу №%d подтверждена! Спасибо за заказ.", mainID)
			_, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: userID,
				Text:   text,
			})
			if err != nil {
				h.logger.Warn("send paid confirmation to user", zap.Error(err))
			}
		}

	// --------- Отклонение оплаты заказа ----------
	case "pay_reject":
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: update.CallbackQuery.ID,
			Text:            "Оплата заказа отклонена ❌",
			ShowAlert:       false,
		})

		if userID != 0 {
			text := fmt.Sprintf(
				"❌ Оплата по заказу №%d не прошла проверку.\n"+
					"Пожалуйста, свяжитесь с администратором или отправьте корректный чек ещё раз.",
				mainID,
			)
			_, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: userID,
				Text:   text,
			})
			if err != nil {
				h.logger.Warn("send reject payment to user", zap.Error(err))
			}
		}

	// --------- Подтверждение оплаты ПОДПИСКИ ----------
	case "sub_ok":
		// mainID — это id из таблицы subscriptions
		if mainID > 0 && userID != 0 {
			now := time.Now()
			validUntil := now.AddDate(0, 1, 0) // +1 месяц

			// активируем подписку
			_, err := h.db.Exec(`
				UPDATE subscriptions
				SET status = 'active', valid_until = ?
				WHERE id = ?
			`, validUntil, mainID)
			if err != nil {
				h.logger.Error("update subscription active", zap.Error(err))
			}

			// проставляем статусы в users
			userIDStr := fmt.Sprint(userID)
			_, err = h.db.Exec(`
				UPDATE users
				SET sub_status = 'active', sub_until = ?
				WHERE user_id = ?
			`, validUntil, userIDStr)
			if err != nil {
				h.logger.Error("update user sub_status active", zap.Error(err))
			}

			// сбрасываем состояние пользователя в Redis
			if h.redisClient != nil {
				state, err := h.redisClient.GetUserState(ctx, userID)
				if err != nil {
					h.logger.Warn("get user state for sub_ok", zap.Error(err))
				}
				if state == nil {
					state = &domain.UserState{}
				}
				state.State = stateStart
				state.IsPaid = true
				if err := h.redisClient.SaveUserState(ctx, userID, state); err != nil {
					h.logger.Warn("save user state after sub_ok", zap.Error(err))
				}
			}

			// ответ админу
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
				CallbackQueryID: update.CallbackQuery.ID,
				Text:            "Подписка активирована ✅",
				ShowAlert:       false,
			})

			// сообщение пользователю
			_, err = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: userID,
				Text: fmt.Sprintf(
					"✅ Ваша подписка на «АГРО Клуб Оптовых Цен» активирована!\n"+
						"Доступ к оптовым ценам до: %s.",
					validUntil.Format("2006-01-02"),
				),
			})
			if err != nil {
				h.logger.Warn("send sub active to user", zap.Error(err))
			}
		}

	// --------- Отклонение оплаты ПОДПИСКИ ----------
	case "sub_reject":
		if mainID > 0 {
			_, err := h.db.Exec(`
				UPDATE subscriptions
				SET status = 'rejected'
				WHERE id = ?
			`, mainID)
			if err != nil {
				h.logger.Error("update subscription rejected", zap.Error(err))
			}
		}

		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: update.CallbackQuery.ID,
			Text:            "Оплата подписки отклонена ❌",
			ShowAlert:       false,
		})

		if userID != 0 {
			_, err := b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: userID,
				Text: "❌ Оплата подписки не прошла проверку.\n" +
					"Пожалуйста, свяжитесь с администратором или отправьте корректный чек ещё раз.",
			})
			if err != nil {
				h.logger.Warn("send sub reject to user", zap.Error(err))
			}
		}
	}
}

// обработка входящего PDF/документа от пользователя в состоянии waiting_payment
// здесь две ветки:
//  1. если есть pending-подписка — отправляем админу как оплату подписки
//  2. иначе — как оплату заказа (старая логика)
func (h *Handler) handlePaymentDocument(ctx context.Context, b *bot.Bot, update *models.Update, state *domain.UserState) error {
	if update.Message == nil || update.Message.Document == nil {
		return nil
	}

	userID := update.Message.From.ID
	userName := update.Message.From.Username
	chatID := update.Message.Chat.ID
	userIDStr := fmt.Sprint(userID)

	// 1) Проверяем, есть ли "ожидающая" подписка для этого пользователя
	var (
		subID      int64
		subAmount  int64
		subPhone   string
		subStatus  string
		validUntil sql.NullTime
	)
	err := h.db.QueryRow(`
		SELECT id, amount, phone, status, valid_until
		FROM subscriptions
		WHERE user_id = ? AND status = 'pending'
		ORDER BY id DESC
		LIMIT 1
	`, userIDStr).Scan(&subID, &subAmount, &subPhone, &subStatus, &validUntil)

	if err == nil && subID > 0 {
		// ===== ЧЕК ПО ПОДПИСКЕ =====
		var sb strings.Builder
		fmt.Fprintf(&sb,
			"💳 Подтверждение оплаты ПОДПИСКИ\n\n"+
				"👤 Пользователь: @%s (ID: %d)\n"+
				"📞 Телефон (из подписки): %s\n"+
				"💰 Сумма: %d ₸\n"+
				"📌 Статус в БД: %s\n\n"+
				"Проверьте чек и подтвердите или отклоните оплату подписки.\n",
			userName,
			userID,
			subPhone,
			subAmount,
			subStatus,
		)

		caption := sb.String()

		cbOK := fmt.Sprintf("sub_ok:%d:%d", subID, userID)
		cbReject := fmt.Sprintf("sub_reject:%d:%d", subID, userID)

		kb := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					{Text: "✅ Активировать подписку", CallbackData: cbOK},
					{Text: "❌ Отклонить", CallbackData: cbReject},
				},
			},
		}

		// копируем сообщение с документом админу
		_, err := b.CopyMessage(ctx, &bot.CopyMessageParams{
			ChatID:      h.cfg.AdminID,
			FromChatID:  fmt.Sprint(chatID),
			MessageID:   update.Message.ID,
			Caption:     caption,
			ReplyMarkup: kb,
		})
		if err != nil {
			h.logger.Error("copy subscription payment doc to admin", zap.Error(err))
			return err
		}

		// уведомляем пользователя
		_, err = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "✅ Чек по подписке отправлен администратору. Мы проверим оплату и сообщим о результате.",
		})
		if err != nil {
			h.logger.Warn("send subscription payment received msg to user", zap.Error(err))
		}
		return nil
	}

	// ===== ЕСЛИ ПОДПИСКИ pending НЕТ — ПАДАЕМ В СТАРУЮ ЛОГИКУ ОПЛАТЫ ЗАКАЗА =====

	// ищем последний заказ пользователя
	var orderID, totalAmount int64

	err = h.db.QueryRow(`SELECT id, total_amount FROM orders WHERE user_id = ? ORDER BY id DESC LIMIT 1`, userIDStr).
		Scan(&orderID, &totalAmount)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		h.logger.Warn("select last order for payment", zap.Error(err))
	}

	payMethod := state.BroadCastType
	if payMethod == "" {
		payMethod = paymentKaspiLink
	}

	// --- Тянем позиции заказа для админа ---
	var itemsText string
	if orderID > 0 {
		rows, errItems := h.db.Query(`
			SELECT name, unit, qty, price, amount
			FROM order_items
			WHERE order_id = ?
			ORDER BY id
		`, orderID)
		if errItems != nil {
			h.logger.Warn("select order items for payment", zap.Error(errItems))
		} else {
			defer rows.Close()
			var sbItems strings.Builder
			var sumItems int64

			for rows.Next() {
				var (
					name   string
					unit   string
					qty    float64
					price  int64
					amount int64
				)
				if err := rows.Scan(&name, &unit, &qty, &price, &amount); err != nil {
					h.logger.Warn("scan order item for payment", zap.Error(err))
					continue
				}
				sumItems += amount
				fmt.Fprintf(&sbItems, "• %s — %.2f %s × %d ₸ = %d ₸\n",
					name, qty, unit, price, amount)
			}

			if sbItems.Len() > 0 {
				itemsText = "\n🛒 Позиции заказа:\n" + sbItems.String() +
					fmt.Sprintf("💰 Сумма по позициям: %d ₸\n", sumItems)
			}
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb,
		"💳 Подтверждение оплаты по заказу №%d\n\n"+
			"👤 Пользователь: @%s (ID: %d)\n"+
			"📞 Телефон: %s\n"+
			"💰 Сумма из orders.total_amount: %d ₸\n"+
			"💳 Способ оплаты: %s\n\n"+
			"Проверьте чек и подтвердите или отклоните оплату.\n",
		orderID,
		userName,
		userID,
		state.Contact,
		totalAmount,
		humanPaymentMethod(payMethod),
	)

	// добавляем детали позиций (если смогли достать)
	if itemsText != "" {
		sb.WriteString("\n")
		sb.WriteString(itemsText)
	}

	caption := sb.String()

	cbOK := fmt.Sprintf("pay_ok:%d:%d", orderID, userID)
	cbReject := fmt.Sprintf("pay_reject:%d:%d", orderID, userID)

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "✅ Подтвердить оплату", CallbackData: cbOK},
				{Text: "❌ Отклонить", CallbackData: cbReject},
			},
		},
	}

	// копируем сообщение с документом админу
	_, err = b.CopyMessage(ctx, &bot.CopyMessageParams{
		ChatID:      h.cfg.AdminID,
		FromChatID:  fmt.Sprint(chatID),
		MessageID:   update.Message.ID,
		Caption:     caption,
		ReplyMarkup: kb,
	})
	if err != nil {
		h.logger.Error("copy payment doc to admin", zap.Error(err))
		return err
	}

	// уведомляем пользователя
	_, err = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   "✅ Чек отправлен администратору. Мы проверим оплату и сообщим о результате.",
	})
	if err != nil {
		h.logger.Warn("send payment received msg to user", zap.Error(err))
	}

	return nil
}

// ======================== HTTP / MINI-APP ========================

func (h *Handler) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Telegram-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) handleDeliveryPrice(w http.ResponseWriter, r *http.Request) {
	// В будущем можно учитывать расстояние, время и т.д.
	// Сейчас — плоская ставка.
	price := int64(1000) // 1000 ₸
	jsonOK(w, map[string]any{
		"price":    price,
		"currency": "KZT",
	})
}

func (h *Handler) StartWebServer(ctx context.Context, b *bot.Bot) {
	h.SetBot(b)

	mux := http.NewServeMux()

	// STATIC pages
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/welcome.html")
	})
	mux.HandleFunc("/catalog", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/catalog.html")
	})
	mux.HandleFunc("/order-confirm", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/order-confirm.html")
	})

	mux.HandleFunc("/admin-add", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/admin-add.html")
	})
	mux.HandleFunc("/admin-add-store", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/admin-add-store.html")
	})
	mux.HandleFunc("/admin-show-catalog", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/admin-show-catalog.html")
	})
	mux.HandleFunc("/admin-edit-product", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/admin-edit-product.html")
	})

	// ❗️Было: "./static/maFp-view.html" (опечатка)
	mux.HandleFunc("/map-view", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/map-view.html")
	})
	mux.HandleFunc("/store-select", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./static/store-select.html")
	})

	// simple admin id source
	mux.HandleFunc("/admin-id", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]any{"adminId": h.cfg.AdminID})
	})

	// STORES
	mux.HandleFunc("/api/stores", h.handleListStores)
	mux.HandleFunc("/api/admin/stores/add", h.handleAddStore)

	// USER / SHOP API
	mux.HandleFunc("/api/user/subscription-status", h.handleGetSubStatus)
	mux.HandleFunc("/api/subscribe/request-invoice", h.handleRequestInvoice)
	mux.HandleFunc("/api/user/set-store", h.handleSetStore)
	mux.HandleFunc("/api/products", h.handleGetProducts)

	// ❗️Оба эндпоинта заказов:
	mux.HandleFunc("/api/orders/create", h.handleCreateOrder)
	mux.HandleFunc("/api/orders/confirm", h.handleConfirmOrder)

	// ADMIN: products
	mux.HandleFunc("/api/admin/products", h.handleAdminListProducts)
	mux.HandleFunc("/api/admin/products/get", h.handleAdminGetProduct)
	mux.HandleFunc("/api/admin/products/add", h.handleAdminAddProduct)
	mux.HandleFunc("/api/admin/products/update", h.handleAdminUpdateProduct)
	mux.HandleFunc("/api/admin/products/delete", h.handleAdminDeleteProduct)

	// Delivery price
	mux.HandleFunc("/api/delivery/price", h.handleDeliveryPrice)

	// uploads static
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir("./uploads"))))

	handler := h.corsMiddleware(mux)
	addr := fmt.Sprintf(":%s", h.cfg.Port)
	h.logger.Info("Web server listening", zap.String("address", addr))

	server := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		h.logger.Info("Shutting down web server...")
		_ = server.Shutdown(context.Background())
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		h.logger.Error("Web server error", zap.Error(err))
	}
}

// =============== Admin helpers ===============
func (h *Handler) isAdminRequest(r *http.Request) bool {
	tgid := strings.TrimSpace(r.Header.Get("X-Telegram-Id"))
	if tgid == "" {
		return false
	}
	return tgid == fmt.Sprint(h.cfg.AdminID)
}

// ========================= STORES =========================

type storeIn struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Address string `json:"address"`
}

func (h *Handler) handleListStores(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(`SELECT code, name, COALESCE(address,'') FROM stores ORDER BY name`)
	if err != nil {
		h.logger.Error("list stores", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}
	defer rows.Close()

	type store struct {
		Code    string `json:"code"`
		Name    string `json:"name"`
		Address string `json:"address"`
	}
	var out []store
	for rows.Next() {
		var s store
		if err := rows.Scan(&s.Code, &s.Name, &s.Address); err != nil {
			h.logger.Error("scan store", zap.Error(err))
			continue
		}
		out = append(out, s)
	}
	jsonOK(w, out)
}

// handler/geocode.go (или в handler.go рядом с Handler)
func (h *Handler) geocodeAddress(addr string) (lng, lat float64, formatted string, err error) {
	if strings.TrimSpace(addr) == "" || h.cfg.YandexAPIKey == "" {
		return 0, 0, "", fmt.Errorf("no address or no api key")
	}
	url := fmt.Sprintf("https://geocode-maps.yandex.ru/1.x/?apikey=%s&geocode=%s&format=json&lang=ru_RU&results=1",
		h.cfg.YandexAPIKey, urlQueryEscape(addr)) // urlQueryEscape = url.QueryEscape
	resp, e := http.Get(url)
	if e != nil {
		return 0, 0, "", e
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	// простенький парс без структур
	var j map[string]any
	if err := json.Unmarshal(b, &j); err != nil {
		return 0, 0, "", err
	}
	g := dig(j, "response", "GeoObjectCollection", "featureMember")
	arr, _ := g.([]any)
	if len(arr) == 0 {
		return 0, 0, "", fmt.Errorf("no results")
	}
	geo, _ := arr[0].(map[string]any)
	obj := dig(geo, "GeoObject").(map[string]any)
	pos := dig(obj, "Point", "pos").(string)
	parts := strings.Fields(pos)
	if len(parts) != 2 {
		return 0, 0, "", fmt.Errorf("bad pos")
	}
	lng, _ = strconv.ParseFloat(parts[0], 64)
	lat, _ = strconv.ParseFloat(parts[1], 64)

	formatted = ""
	if md, ok := dig(obj, "metaDataProperty", "GeocoderMetaData").(map[string]any); ok {
		if a, ok := md["Address"].(map[string]any); ok {
			if f, ok := a["formatted"].(string); ok {
				formatted = f
			}
		}
		if formatted == "" {
			if t, ok := md["text"].(string); ok {
				formatted = t
			}
		}
	}
	return lng, lat, formatted, nil
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "+"), "\n", "+")
}

func dig(m any, path ...string) any {
	cur := m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
		if cur == nil {
			return nil
		}
	}
	return cur
}

func (h *Handler) handleAddStore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.isAdminRequest(r) {
		jsonErr(w, http.StatusForbidden, "forbidden")
		return
	}

	var in storeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, 400, "invalid json")
		return
	}
	in.Code = strings.TrimSpace(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	in.Address = strings.TrimSpace(in.Address)
	if in.Code == "" || in.Name == "" {
		jsonErr(w, 400, "code and name are required")
		return
	}

	var lng, lat float64
	var formatted string
	if in.Address != "" && h.cfg.YandexAPIKey != "" {
		l, a, f, err := h.geocodeAddress(in.Address)
		if err == nil {
			lng, lat, formatted = l, a, f
		}
	}

	_, err := h.db.Exec(`
        INSERT INTO stores(code,name,address,longitude,latitude,address_formatted)
        VALUES(?,?,?,?,?,?)
        ON CONFLICT(code) DO UPDATE SET
           name=excluded.name,
           address=excluded.address,
           longitude=excluded.longitude,
           latitude=excluded.latitude,
           address_formatted=excluded.address_formatted
    `, in.Code, in.Name, in.Address, nullIfZero(lng), nullIfZero(lat), sql.NullString{String: formatted, Valid: formatted != ""})
	if err != nil {
		h.logger.Error("insert store", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

func nullIfZero(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ========================= API HANDLERS =========================

func (h *Handler) handleConfirmOrder(w http.ResponseWriter, r *http.Request) {
	var in confirmOrderIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid json")
		return
	}

	var tgStr string
	if err := json.Unmarshal(in.TelegramID, &tgStr); err != nil {
		var tgNum json.Number
		if err2 := json.Unmarshal(in.TelegramID, &tgNum); err2 == nil {
			if i, e := tgNum.Int64(); e == nil {
				tgStr = strconv.FormatInt(i, 10)
			}
		}
	}
	tgStr = strings.TrimSpace(tgStr)
	if tgStr == "" || len(in.Items) == 0 {
		jsonErr(w, http.StatusBadRequest, "telegram_id and items are required")
		return
	}

	payMethod := strings.TrimSpace(in.PaymentMethod)
	if payMethod == "" {
		payMethod = paymentKaspiLink
	}

	// Проверим выбранный магазин (как и в handleCreateOrder)
	var store sql.NullString
	_ = h.db.QueryRow(`SELECT selected_store FROM users WHERE user_id = ?`, tgStr).Scan(&store)

	// Базовая сумма
	var goodsTotal int64
	for _, it := range in.Items {
		if it.Qty <= 0 || it.Price < 0 {
			jsonErr(w, http.StatusBadRequest, "bad item qty/price")
			return
		}
		goodsTotal += int64(it.Qty * float64(it.Price))
	}

	// Цена доставки (если выбрана доставка)
	var deliveryPrice int64
	if strings.EqualFold(in.Delivery.Type, "delivery") {
		// сейчас — плоская ставка 1000 ₸ (как в /api/delivery/price)
		deliveryPrice = 1000
		// добавим как строку заказа «Доставка»
		in.Items = append(in.Items, orderItemIn{
			ProductID: 0,
			Name:      "Доставка",
			Qty:       1,
			Unit:      "услуга",
			Price:     deliveryPrice,
		})
	}

	total := goodsTotal + deliveryPrice

	// Транзакция
	tx, err := h.db.Begin()
	if err != nil {
		h.logger.Error("tx begin", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
		INSERT INTO orders (user_id, store_code, total_amount, status)
		VALUES (?, ?, ?, 'new')
	`, tgStr, nullIfEmpty(store.String), total)
	if err != nil {
		h.logger.Error("insert order", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}
	orderID, _ := res.LastInsertId()

	stmt, err := tx.Prepare(`
		INSERT INTO order_items (order_id, product_id, name, unit, qty, price, amount)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		h.logger.Error("prepare order items", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}
	defer stmt.Close()

	for _, it := range in.Items {
		amount := int64(it.Qty * float64(it.Price))
		if _, err := stmt.Exec(orderID, it.ProductID, it.Name, it.Unit, it.Qty, it.Price, amount); err != nil {
			h.logger.Error("insert order item", zap.Error(err))
			jsonErr(w, 500, "db error")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.logger.Error("tx commit", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}

	// Сохраняем состояние пользователя в Redis: ждём оплаты
	if h.redisClient != nil {
		if uid, err := strconv.ParseInt(tgStr, 10, 64); err == nil {
			st := &domain.UserState{
				State:         stateWaitingPayment,
				BroadCastType: payMethod,         // способ оплаты
				Contact:       in.Delivery.Phone, // телефон клиента
				IsPaid:        false,
				Count:         0,
			}
			if err := h.redisClient.SaveUserState(h.ctx, uid, st); err != nil {
				h.logger.Warn("save user state to redis", zap.Error(err))
			}
		}
	}

	// ⚠️ Уведомление админу с деталями доставки
	{
		var b strings.Builder
		fmt.Fprintf(&b, "🧾 Новый заказ (подтверждён)\n\n")
		fmt.Fprintf(&b, "👤 Telegram ID: %s\n", tgStr)

		if store.Valid && store.String != "" {
			var name, addr sql.NullString
			_ = h.db.QueryRow(`SELECT name, address FROM stores WHERE code = ?`, store.String).Scan(&name, &addr)
			if name.Valid {
				fmt.Fprintf(&b, "🏪 Точка: %s\n", name.String)
			}
			if addr.Valid {
				fmt.Fprintf(&b, "📍 Адрес точки: %s\n", addr.String)
			}
		}

		fmt.Fprintf(&b, "💳 Способ оплаты: %s\n", humanPaymentMethod(payMethod))

		if strings.EqualFold(in.Delivery.Type, "delivery") {
			fmt.Fprintf(&b, "🚚 Доставка на дом\n")
			if strings.TrimSpace(in.Delivery.Address) != "" {
				fmt.Fprintf(&b, "📬 Адрес клиента: %s\n", in.Delivery.Address)
			}
		} else {
			fmt.Fprintf(&b, "🏃 Самовывоз\n")
		}
		if strings.TrimSpace(in.Delivery.Phone) != "" {
			fmt.Fprintf(&b, "📞 Телефон клиента: %s\n", in.Delivery.Phone)
		}

		fmt.Fprintf(&b, "\n🛒 Позиции:\n")
		for _, it := range in.Items {
			fmt.Fprintf(&b, "• %s — %.2f (%s) × %d ₸\n", it.Name, it.Qty, it.Unit, it.Price)
		}
		fmt.Fprintf(&b, "💰 Сумма (включая доставку): %d ₸", total)

		h.notifyAdmin(b.String())
	}

	// Чек пользователю
	if err := h.sendOrderReceiptToUser(tgStr, orderID, in.Items, total, store.String, payMethod); err != nil {
		h.logger.Warn("send receipt to user", zap.Error(err))
	}

	jsonOK(w, map[string]any{
		"status":         "ok",
		"order_id":       orderID,
		"goods_total":    goodsTotal,
		"delivery_price": deliveryPrice,
		"total":          total,
	})
}

func (h *Handler) handleGetSubStatus(w http.ResponseWriter, r *http.Request) {
	telegramID := firstNonEmpty(
		r.URL.Query().Get("telegram_id"),
		r.Header.Get("X-Telegram-Id"),
	)
	if telegramID == "" {
		jsonErr(w, http.StatusBadRequest, "telegram_id is required")
		return
	}

	var subStatus string
	var subUntil sql.NullTime
	var selectedStore sql.NullString

	err := h.db.QueryRow(`
		SELECT sub_status, sub_until, selected_store
		FROM users
		WHERE user_id = ?
	`, telegramID).Scan(&subStatus, &subUntil, &selectedStore)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		h.logger.Error("select users sub", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}

	active := false
	until := ""
	now := time.Now()
	if subStatus == "active" && subUntil.Valid && subUntil.Time.After(now) {
		active = true
		until = subUntil.Time.Format("2006-01-02")
	} else {
		// смотрим последнюю активную подписку в subscriptions
		_ = h.db.QueryRow(`
			SELECT valid_until
			FROM subscriptions
			WHERE user_id = ? AND status = 'active'
			ORDER BY valid_until DESC
			LIMIT 1
		`, telegramID).Scan(&subUntil)
		if subUntil.Valid && subUntil.Time.After(now) {
			active = true
			until = subUntil.Time.Format("2006-01-02")
		}
	}

	var storeName, storeAddr sql.NullString
	var storeLng, storeLat sql.NullFloat64
	var addrFmt sql.NullString

	if selectedStore.Valid && selectedStore.String != "" {
		_ = h.db.QueryRow(`
            SELECT name, COALESCE(address,''), longitude, latitude, COALESCE(address_formatted,'')
            FROM stores WHERE code = ?`,
			selectedStore.String,
		).Scan(&storeName, &storeAddr, &storeLng, &storeLat, &addrFmt)
	}

	jsonOK(w, map[string]any{
		"active":        active,
		"until":         until,
		"store_code":    selectedStore.String,
		"store_name":    storeName.String,
		"store_address": firstNonEmpty(addrFmt.String, storeAddr.String),
		"store_lng":     storeLng.Float64,
		"store_lat":     storeLat.Float64,
	})
}

type requestInvoiceIn struct {
	TelegramID string `json:"telegram_id"`
	Phone      string `json:"phone"`
}

func (h *Handler) handleRequestInvoice(w http.ResponseWriter, r *http.Request) {
	var in requestInvoiceIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.TelegramID = strings.TrimSpace(in.TelegramID)
	in.Phone = strings.TrimSpace(in.Phone)
	if in.TelegramID == "" || in.Phone == "" {
		jsonErr(w, http.StatusBadRequest, "telegram_id and phone are required")
		return
	}

	// upsert user + помечаем sub_status = pending
	uid := uuid.New().String()
	_, err := h.db.Exec(`
		INSERT INTO users (id, user_id, nickname, phone, sub_status)
		VALUES (?, ?, COALESCE((SELECT nickname FROM users WHERE user_id = ?),'user'), ?, 'pending')
		ON CONFLICT(user_id) DO UPDATE SET
		  phone = excluded.phone,
		  sub_status = 'pending',
		  updated_at = CURRENT_TIMESTAMP
	`, uid, in.TelegramID, in.TelegramID, in.Phone)
	if err != nil {
		h.logger.Error("upsert users phone", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}

	// создаём запись в subscriptions
	_, err = h.db.Exec(`
		INSERT INTO subscriptions (user_id, phone, status, amount)
		VALUES (?, ?, 'pending', 3000)
	`, in.TelegramID, in.Phone)
	if err != nil {
		h.logger.Error("insert subscription", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}

	// сохраняем состояние "ждём чек по подписке" в Redis
	if h.redisClient != nil {
		if tgid, err := strconv.ParseInt(in.TelegramID, 10, 64); err == nil {
			st := &domain.UserState{
				State:         stateWaitingPayment,
				BroadCastType: paymentKaspiLink,
				Contact:       in.Phone,
				IsPaid:        false,
			}
			if err := h.redisClient.SaveUserState(h.ctx, tgid, st); err != nil {
				h.logger.Warn("save user state wait sub payment", zap.Error(err))
			}
		}
	}

	// отправляем админу уведомление
	h.notifyAdmin(fmt.Sprintf(
		"🧾 Заявка на подписку\n\n👤 Telegram ID: %s\n📞 Телефон: %s\nСумма: 3000 ₸\n\nПользователь получил ссылку Kaspi Pay и должен прислать чек. После чека — подтвердите подписку.",
		in.TelegramID, in.Phone,
	))

	// отправляем пользователю ссылку на оплату через бота
	if h.bot != nil {
		if tgid, err := strconv.ParseInt(in.TelegramID, 10, 64); err == nil {
			kaspiURL := strings.TrimSpace(h.cfg.KaspiPayURL)
			if kaspiURL == "" {
				kaspiURL = "https://pay.kaspi.kz/pay/e96vsxbs"
			}

			text := "💳 Подписка АГРО Клуб — 3000 ₸/мес.\n\n" +
				"Перейдите по ссылке Kaspi Pay и оплатите подписку, затем отправьте сюда чек (PDF или скриншот), " +
				"чтобы администратор подтвердил оплату.\n"

			kb := &models.InlineKeyboardMarkup{
				InlineKeyboard: [][]models.InlineKeyboardButton{
					{
						{Text: "💳 Оплатить подписку", URL: kaspiURL},
					},
				},
			}

			_, err = h.bot.SendMessage(h.ctx, &bot.SendMessageParams{
				ChatID:      tgid,
				Text:        text,
				ReplyMarkup: kb,
			})
			if err != nil {
				h.logger.Warn("send kaspi link to user", zap.Error(err))
			}
		}
	}

	jsonOK(w, map[string]string{"status": "ok"})
}

type setStoreIn struct {
	TelegramID string `json:"telegram_id"`
	Store      string `json:"store"`
}

func (h *Handler) handleSetStore(w http.ResponseWriter, r *http.Request) {
	var in setStoreIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.TelegramID = strings.TrimSpace(in.TelegramID)
	in.Store = strings.TrimSpace(in.Store)
	if in.TelegramID == "" || in.Store == "" {
		jsonErr(w, http.StatusBadRequest, "telegram_id and store are required")
		return
	}

	// ensure store exists
	var cnt int
	_ = h.db.QueryRow(`SELECT COUNT(1) FROM stores WHERE code = ? OR name = ?`, in.Store, in.Store).Scan(&cnt)
	if cnt == 0 {
		jsonErr(w, 400, "store not found")
		return
	}

	uid := uuid.New().String()
	_, err := h.db.Exec(`
		INSERT INTO users (id, user_id, nickname, selected_store)
		VALUES (?, ?, COALESCE((SELECT nickname FROM users WHERE user_id = ?),'user'), ?)
		ON CONFLICT(user_id) DO UPDATE SET
		  selected_store = excluded.selected_store,
		  updated_at = CURRENT_TIMESTAMP
	`, uid, in.TelegramID, in.TelegramID, in.Store)
	if err != nil {
		h.logger.Error("update selected_store", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}

	jsonOK(w, map[string]string{"status": "ok"})
}

func (h *Handler) handleGetProducts(w http.ResponseWriter, r *http.Request) {
	// опционально фильтруем по store_code, если у пользователя выбран магазин (X-Telegram-Id)
	tgid := strings.TrimSpace(r.Header.Get("X-Telegram-Id"))
	var store sql.NullString
	if tgid != "" {
		_ = h.db.QueryRow(`SELECT selected_store FROM users WHERE user_id = ?`, tgid).Scan(&store)
	}

	var rows *sql.Rows
	var err error
	if store.Valid && store.String != "" {
		rows, err = h.db.Query(`
			SELECT id, name, COALESCE(emoji,''), category_slug, unit, price, COALESCE(photo_path,''), COALESCE(store_code,'')
			FROM products
			WHERE active = 1 AND (store_code = ? OR store_code IS NULL OR store_code = '')
			ORDER BY category_slug, name
		`, store.String)
	} else {
		rows, err = h.db.Query(`
			SELECT id, name, COALESCE(emoji,''), category_slug, unit, price, COALESCE(photo_path,''), COALESCE(store_code,'')
			FROM products
			WHERE active = 1
			ORDER BY category_slug, name
		`)
	}
	if err != nil {
		h.logger.Error("select products", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}
	defer rows.Close()

	type product struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Emoji    string `json:"emoji"`
		Category string `json:"category"`
		Unit     string `json:"unit"`
		Price    int64  `json:"price"`
		Photo    string `json:"photo"`
		Store    string `json:"store_code"`
	}

	var out []product
	for rows.Next() {
		var p product
		if err := rows.Scan(&p.ID, &p.Name, &p.Emoji, &p.Category, &p.Unit, &p.Price, &p.Photo, &p.Store); err != nil {
			h.logger.Error("scan product", zap.Error(err))
			continue
		}
		out = append(out, p)
	}

	jsonOK(w, out)
}

func (h *Handler) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	var in createOrderIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid json")
		return
	}

	var tgStr string
	if err := json.Unmarshal(in.TelegramID, &tgStr); err != nil {
		var tgNum json.Number
		if err2 := json.Unmarshal(in.TelegramID, &tgNum); err2 == nil {
			if i, e := tgNum.Int64(); e == nil {
				tgStr = strconv.FormatInt(i, 10)
			}
		}
	}
	tgStr = strings.TrimSpace(tgStr)
	if tgStr == "" || len(in.Items) == 0 {
		jsonErr(w, http.StatusBadRequest, "telegram_id and items are required")
		return
	}

	// Получим магазин пользователя
	var store sql.NullString
	_ = h.db.QueryRow(`SELECT selected_store FROM users WHERE user_id = ?`, tgStr).Scan(&store)

	// Транзакция создания заказа
	tx, err := h.db.Begin()
	if err != nil {
		h.logger.Error("tx begin", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var total int64
	for _, it := range in.Items {
		if it.Qty <= 0 || it.Price < 0 {
			jsonErr(w, http.StatusBadRequest, "bad item qty/price")
			return
		}
		total += int64(it.Qty * float64(it.Price))
	}

	res, err := tx.Exec(`
		INSERT INTO orders (user_id, store_code, total_amount, status)
		VALUES (?, ?, ?, 'new')
	`, tgStr, nullIfEmpty(store.String), total)
	if err != nil {
		h.logger.Error("insert order", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}
	orderID, _ := res.LastInsertId()

	stmt, err := tx.Prepare(`
		INSERT INTO order_items (order_id, product_id, name, unit, qty, price, amount)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		h.logger.Error("prepare order items", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}
	defer stmt.Close()

	for _, it := range in.Items {
		amount := int64(it.Qty * float64(it.Price))
		if _, err := stmt.Exec(orderID, it.ProductID, it.Name, it.Unit, it.Qty, it.Price, amount); err != nil {
			h.logger.Error("insert order item", zap.Error(err))
			jsonErr(w, http.StatusInternalServerError, "db error")
			return
		}
	}

	if err := tx.Commit(); err != nil {
		h.logger.Error("tx commit", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}

	// Уведомление админу
	{
		var b strings.Builder
		fmt.Fprintf(&b, "🧾 Новый заказ\n\n")
		fmt.Fprintf(&b, "👤 Telegram ID: %s\n", tgStr)
		if store.Valid && store.String != "" {
			var name, addr sql.NullString
			_ = h.db.QueryRow(`SELECT name, address FROM stores WHERE code = ?`, store.String).Scan(&name, &addr)
			if name.Valid {
				fmt.Fprintf(&b, "🏪 Магазин: %s\n", name.String)
			}
			if addr.Valid {
				fmt.Fprintf(&b, "📍 Адрес: %s\n", addr.String)
			}
		}
		fmt.Fprintf(&b, "🛒 Позиции:\n")
		for _, it := range in.Items {
			fmt.Fprintf(&b, "• %s — %.2f (%s) × %d ₸\n", it.Name, it.Qty, it.Unit, it.Price)
		}
		fmt.Fprintf(&b, "💰 Сумма: %d ₸", total)

		h.notifyAdmin(b.String())
	}

	// Чек пользователю с кнопкой Kaspi Pay (по умолчанию kaspi_link)
	if err := h.sendOrderReceiptToUser(tgStr, orderID, in.Items, total, store.String, paymentKaspiLink); err != nil {
		h.logger.Warn("send receipt to user", zap.Error(err))
	}

	jsonOK(w, map[string]any{"status": "ok", "order_id": orderID, "total": total})
}

// Формирует и отправляет пользователю сообщение с позициями, суммой и способом оплаты.
func (h *Handler) sendOrderReceiptToUser(telegramID string, orderID int64, items []orderItemIn, total int64, storeCode string, paymentMethod string) error {
	if h.bot == nil {
		return fmt.Errorf("bot is nil")
	}

	// 1) Преобразуем telegramID -> int64 ChatID
	tgStr := strings.TrimSpace(telegramID)
	if tgStr == "" {
		return fmt.Errorf("empty telegram id")
	}
	tgid, err := strconv.ParseInt(tgStr, 10, 64)
	if err != nil {
		return fmt.Errorf("bad telegram id: %w", err)
	}

	// 2) Достанем информацию о точке (если есть)
	var storeName, storeAddr string
	if strings.TrimSpace(storeCode) != "" {
		_ = h.db.QueryRow(
			`SELECT COALESCE(name,''), COALESCE(address,'') FROM stores WHERE code = ?`,
			storeCode,
		).Scan(&storeName, &storeAddr)
	}

	if paymentMethod == "" {
		paymentMethod = paymentKaspiLink
	}

	// 3) Сформируем текст чека
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Заказ №%d принят!\n\n", orderID)
	if storeName != "" {
		fmt.Fprintf(&b, "🏪 Точка: %s\n", storeName)
	}
	if storeAddr != "" {
		fmt.Fprintf(&b, "📍 Адрес: %s\n", storeAddr)
	}

	fmt.Fprintf(&b, "💳 Способ оплаты: %s\n\n", humanPaymentMethod(paymentMethod))

	b.WriteString("🛒 Позиции:\n")

	var calcTotal int64
	for _, it := range items {
		if it.Qty <= 0 || it.Price < 0 {
			continue
		}
		lineAmount := int64(it.Qty * float64(it.Price))
		calcTotal += lineAmount

		fmt.Fprintf(&b, "• %s — %.2f %s × %d ₸ = %d ₸\n",
			it.Name, it.Qty, it.Unit, it.Price, lineAmount)
	}

	if calcTotal == 0 && total > 0 {
		calcTotal = total
	}

	fmt.Fprintf(&b, "\n💰 Итого к оплате: %d ₸\n", calcTotal)

	// ReplyMarkup
	var kb models.ReplyMarkup
	switch paymentMethod {
	case paymentKaspiLink:
		kaspiURL := h.cfg.KaspiPayURL
		if strings.TrimSpace(kaspiURL) == "" {
			kaspiURL = "https://pay.kaspi.kz/pay/e96vsxbs"
		}
		kb = &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					{Text: "💳 Оплатить в Kaspi", URL: kaspiURL},
				},
			},
		}
	case paymentKaspiTransfer:
		b.WriteString("\n📌 Реквизиты для перевода на Kaspi Gold:\n")
		fmt.Fprintf(&b, "Номер карты: %s\n", kaspiGoldNumber)
		fmt.Fprintf(&b, "Получатель: %s\n\n", kaspiGoldOwnerName)
		b.WriteString("После оплаты, пожалуйста, отправьте сюда PDF или скрин чека, чтобы мы могли подтвердить платеж ✅.\n")
	case paymentCash:
		b.WriteString("\n💵 Оплата наличными при получении заказа.\n")
	default:
		kaspiURL := h.cfg.KaspiPayURL
		if strings.TrimSpace(kaspiURL) == "" {
			kaspiURL = "https://pay.kaspi.kz/pay/e96vsxbs"
		}
		kb = &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					{Text: "💳 Оплатить в Kaspi", URL: kaspiURL},
				},
			},
		}
	}

	// 5) Отправка сообщения пользователю
	params := &bot.SendMessageParams{
		ChatID: tgid,
		Text:   b.String(),
	}
	if kb != nil {
		params.ReplyMarkup = kb
	}

	_, err = h.bot.SendMessage(h.ctx, params)
	return err
}

// ========================= ADMIN PRODUCTS =========================

func (h *Handler) handleAdminListProducts(w http.ResponseWriter, r *http.Request) {
	if !h.isAdminRequest(r) {
		jsonErr(w, http.StatusForbidden, "forbidden")
		return
	}
	rows, err := h.db.Query(`
		SELECT id, name, category_slug, unit, price, active, COALESCE(photo_path,''), COALESCE(description,''), COALESCE(store_code,'')
		FROM products
		ORDER BY category_slug, name
	`)
	if err != nil {
		h.logger.Error("admin list products", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}
	defer rows.Close()

	type product struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Category    string `json:"category"`
		Unit        string `json:"unit"`
		Price       int64  `json:"price"`
		Active      int64  `json:"active"`
		Photo       string `json:"photo"`
		Description string `json:"description"`
		Store       string `json:"store_code"`
	}
	var out []product
	for rows.Next() {
		var p product
		if err := rows.Scan(&p.ID, &p.Name, &p.Category, &p.Unit, &p.Price, &p.Active, &p.Photo, &p.Description, &p.Store); err != nil {
			h.logger.Error("scan product", zap.Error(err))
			continue
		}
		out = append(out, p)
	}
	jsonOK(w, out)
}

func (h *Handler) handleAdminGetProduct(w http.ResponseWriter, r *http.Request) {
	if !h.isAdminRequest(r) {
		jsonErr(w, http.StatusForbidden, "forbidden")
		return
	}
	idStr := strings.TrimSpace(r.URL.Query().Get("id"))
	if idStr == "" {
		jsonErr(w, 400, "id required")
		return
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	var p struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Category    string `json:"category"`
		Unit        string `json:"unit"`
		Price       int64  `json:"price"`
		Active      int64  `json:"active"`
		Photo       string `json:"photo"`
		Description string `json:"description"`
		Store       string `json:"store_code"`
	}
	err := h.db.QueryRow(`
		SELECT id, name, category_slug, unit, price, active, COALESCE(photo_path,''), COALESCE(description,''), COALESCE(store_code,'')
		FROM products WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &p.Category, &p.Unit, &p.Price, &p.Active, &p.Photo, &p.Description, &p.Store,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonErr(w, 404, "not found")
			return
		}
		h.logger.Error("get product", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}
	jsonOK(w, p)
}

func (h *Handler) handleAdminUpdateProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.isAdminRequest(r) {
		jsonErr(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		jsonErr(w, 400, "invalid multipart form")
		return
	}

	idStr := strings.TrimSpace(r.FormValue("id"))
	if idStr == "" {
		jsonErr(w, 400, "id required")
		return
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)

	name := strings.TrimSpace(r.FormValue("name"))
	cat := strings.TrimSpace(r.FormValue("category"))
	unit := strings.TrimSpace(r.FormValue("unit"))
	priceStr := strings.TrimSpace(r.FormValue("price"))
	activeStr := strings.TrimSpace(r.FormValue("active"))
	desc := strings.TrimSpace(r.FormValue("description"))
	storeCode := strings.TrimSpace(r.FormValue("store_code"))
	removePhoto := strings.TrimSpace(r.FormValue("remove_photo")) == "1"

	if name == "" || cat == "" || unit == "" || priceStr == "" || storeCode == "" {
		jsonErr(w, 400, "name, category, unit, price, store_code are required")
		return
	}

	// validate store exists
	var cnt int
	_ = h.db.QueryRow(`SELECT COUNT(1) FROM stores WHERE code = ?`, storeCode).Scan(&cnt)
	if cnt == 0 {
		jsonErr(w, 400, "store not found")
		return
	}

	price, _ := strconv.ParseInt(priceStr, 10, 64)
	if price < 0 {
		jsonErr(w, 400, "price must be >= 0")
		return
	}
	active := int64(1)
	if activeStr == "0" {
		active = 0
	}

	// Load current photo
	var oldPhoto sql.NullString
	_ = h.db.QueryRow(`SELECT photo_path FROM products WHERE id = ?`, id).Scan(&oldPhoto)

	// If new photo uploaded
	newPhoto := oldPhoto.String
	file, header, err := r.FormFile("photo")
	if err == nil && header != nil {
		defer file.Close()
		if path, e := saveUpload(file, header); e == nil {
			newPhoto = path
			if oldPhoto.Valid && oldPhoto.String != "" {
				_ = os.Remove("." + oldPhoto.String)
			}
		}
	}
	// If remove flag set
	if removePhoto {
		if oldPhoto.Valid && oldPhoto.String != "" {
			_ = os.Remove("." + oldPhoto.String)
		}
		newPhoto = ""
	}

	_, err = h.db.Exec(`
		UPDATE products SET
		  name = ?, category_slug = ?, unit = ?, price = ?, active = ?, description = ?, photo_path = ?, store_code = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		name, cat, unit, price, active, desc, newPhoto, storeCode, id,
	)
	if err != nil {
		h.logger.Error("update product", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}

	jsonOK(w, map[string]string{"status": "ok"})
}

type delReq struct {
	ID int64 `json:"id"`
}

func (h *Handler) handleAdminDeleteProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.isAdminRequest(r) {
		jsonErr(w, http.StatusForbidden, "forbidden")
		return
	}
	var in delReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID <= 0 {
		jsonErr(w, 400, "invalid json")
		return
	}
	// remove photo file if exists
	var photo sql.NullString
	_ = h.db.QueryRow(`SELECT photo_path FROM products WHERE id = ?`, in.ID).Scan(&photo)
	if photo.Valid && photo.String != "" {
		_ = os.Remove("." + photo.String)
	}
	_, err := h.db.Exec(`DELETE FROM products WHERE id = ?`, in.ID)
	if err != nil {
		h.logger.Error("delete product", zap.Error(err))
		jsonErr(w, 500, "db error")
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// ============= ADMIN ADD PRODUCT (multipart) =============

func (h *Handler) handleAdminAddProduct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.isAdminRequest(r) {
		jsonErr(w, http.StatusForbidden, "forbidden")
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil { // 10 MB
		jsonErr(w, http.StatusBadRequest, "invalid multipart form")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	emoji := strings.TrimSpace(r.FormValue("emoji"))
	cat := strings.TrimSpace(r.FormValue("category"))
	unit := strings.TrimSpace(r.FormValue("unit"))
	priceStr := strings.TrimSpace(r.FormValue("price"))
	activeStr := strings.TrimSpace(r.FormValue("active"))
	desc := strings.TrimSpace(r.FormValue("description"))
	storeCode := strings.TrimSpace(r.FormValue("store_code"))

	if name == "" || cat == "" || unit == "" || priceStr == "" || storeCode == "" {
		jsonErr(w, http.StatusBadRequest, "name, category, unit, price, store_code are required")
		return
	}

	// validate store exists
	var cnt int
	_ = h.db.QueryRow(`SELECT COUNT(1) FROM stores WHERE code = ?`, storeCode).Scan(&cnt)
	if cnt == 0 {
		jsonErr(w, 400, "store not found")
		return
	}

	price, _ := strconv.ParseInt(priceStr, 10, 64)
	if price < 0 {
		jsonErr(w, http.StatusBadRequest, "price must be >= 0")
		return
	}
	active := int64(1)
	if activeStr == "0" {
		active = 0
	}

	photoPath := ""
	file, header, err := r.FormFile("photo")
	if err == nil && header != nil {
		defer file.Close()
		photoPath, err = saveUpload(file, header)
		if err != nil {
			h.logger.Warn("save photo error", zap.Error(err))
		}
	}

	_, err = h.db.Exec(`
		INSERT INTO products (name, emoji, category_slug, unit, price, active, description, photo_path, store_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, name, emoji, cat, unit, price, active, desc, photoPath, storeCode)
	if err != nil {
		h.logger.Error("insert product", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}

	h.notifyAdmin(fmt.Sprintf("➕ Добавлен товар\n\n%s %s\nКатегория: %s\nЦена: %d %s\nТочка: %s",
		emoji, name, cat, price, unit, storeCode,
	))

	jsonOK(w, map[string]string{"status": "ok"})
}

// ========================= ORDERS =========================

type orderItemIn struct {
	ProductID int64   `json:"product_id"`
	Name      string  `json:"name"`
	Qty       float64 `json:"qty"`
	Unit      string  `json:"unit"`
	Price     int64   `json:"price"`
}

type createOrderIn struct {
	TelegramID json.RawMessage `json:"telegram_id"`
	Items      []orderItemIn   `json:"items"`
}

// ========================= HELPERS =========================

func (h *Handler) notifyAdmin(text string) {
	if h.bot == nil || h.cfg == nil || h.cfg.AdminID == 0 {
		return
	}
	go func() {
		_, err := h.bot.SendMessage(h.ctx, &bot.SendMessageParams{
			ChatID: h.cfg.AdminID,
			Text:   text,
		})
		if err != nil {
			log.Println("notifyAdmin error:", err)
		}
	}()
}

func saveUpload(file multipart.File, header *multipart.FileHeader) (string, error) {
	if err := os.MkdirAll("./uploads", 0o755); err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == "" {
		ext = ".jpg"
	}
	name := fmt.Sprintf("%s%s", uuid.New().String(), ext)
	dst := filepath.Join("./uploads", name)

	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		return "", err
	}
	return "/uploads/" + name, nil
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": msg,
	})
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func humanPaymentMethod(m string) string {
	switch m {
	case paymentKaspiTransfer:
		return "Kaspi Gold (перевод)"
	case paymentCash:
		return "Наличные"
	default:
		return "Kaspi Pay (ссылка)"
	}
}
