package handler

import (
	"agro/config"
	"agro/internal/repository"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (h *Handler) DefaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}

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
	mux.HandleFunc("/api/stores", h.handleListStores)         // GET list
	mux.HandleFunc("/api/admin/stores/add", h.handleAddStore) // POST {code,name,address}

	// USER / SHOP API
	mux.HandleFunc("/api/user/subscription-status", h.handleGetSubStatus) // now returns store info
	mux.HandleFunc("/api/subscribe/request-invoice", h.handleRequestInvoice)
	mux.HandleFunc("/api/user/set-store", h.handleSetStore)
	mux.HandleFunc("/api/products", h.handleGetProducts)
	mux.HandleFunc("/api/orders/create", h.handleCreateOrder)

	// ADMIN: products
	mux.HandleFunc("/api/admin/products", h.handleAdminListProducts)
	mux.HandleFunc("/api/admin/products/get", h.handleAdminGetProduct)
	mux.HandleFunc("/api/admin/products/add", h.handleAdminAddProduct)       // multipart (+ store_code)
	mux.HandleFunc("/api/admin/products/update", h.handleAdminUpdateProduct) // multipart (+ store_code)
	mux.HandleFunc("/api/admin/products/delete", h.handleAdminDeleteProduct)

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
	if subStatus == "active" && subUntil.Valid && subUntil.Time.After(time.Now()) {
		active = true
		until = subUntil.Time.Format("2006-01-02")
	} else {
		_ = h.db.QueryRow(`
			SELECT valid_until
			FROM subscriptions
			WHERE user_id = ? AND status = 'active'
			ORDER BY valid_until DESC
			LIMIT 1
		`, telegramID).Scan(&subUntil)
		if subUntil.Valid && subUntil.Time.After(time.Now()) {
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
		"store_address": firstNonEmpty(addrFmt.String, storeAddr.String), // отдаём форматированный, если есть
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

	_, err = h.db.Exec(`
		INSERT INTO subscriptions (user_id, phone, status, amount)
		VALUES (?, ?, 'pending', 3000)
	`, in.TelegramID, in.Phone)
	if err != nil {
		h.logger.Error("insert subscription", zap.Error(err))
		jsonErr(w, http.StatusInternalServerError, "db error")
		return
	}

	h.notifyAdmin(fmt.Sprintf(
		"🧾 Заявка на подписку\n\n👤 Telegram ID: %s\n📞 Телефон: %s\nСумма: 3000 ₸\n\nНужно выставить счёт в Kaspi и активировать.",
		in.TelegramID, in.Phone,
	))

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
	_ = h.db.QueryRow(`SELECT selected_store FROM users WHERE user_id = ?`, in.TelegramID).Scan(&store)

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
	`, in.TelegramID, nullIfEmpty(store.String), total)
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
		fmt.Fprintf(&b, "👤 Telegram ID: %s\n", in.TelegramID)
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

	// Чек пользователю с кнопкой Kaspi Pay
	if err := h.sendOrderReceiptToUser(tgStr, orderID, in.Items, total, store.String); err != nil {
		h.logger.Warn("send receipt to user", zap.Error(err))
		// Не фейлим заказ из-за проблем с отправкой сообщения
	}

	jsonOK(w, map[string]any{"status": "ok", "order_id": orderID, "total": total})
}

// Формирует и отправляет пользователю сообщение с позициями, суммой и кнопкой "Оплатить Kaspi".
func (h *Handler) sendOrderReceiptToUser(telegramID string, orderID int64, items []orderItemIn, total int64, storeCode string) error {
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

	// 3) Сформируем текст чека
	var b strings.Builder
	fmt.Fprintf(&b, "✅ Заказ №%d принят!\n\n", orderID)
	if storeName != "" {
		fmt.Fprintf(&b, "🏪 Точка: %s\n", storeName)
	}
	if storeAddr != "" {
		fmt.Fprintf(&b, "📍 Адрес: %s\n", storeAddr)
	}
	b.WriteString("🛒 Позиции:\n")

	var calcTotal int64
	for _, it := range items {
		// защита от мусора
		if it.Qty <= 0 || it.Price < 0 {
			continue
		}
		lineAmount := int64(it.Qty * float64(it.Price))
		calcTotal += lineAmount

		// Пример: • Картофель — 3.00 кг × 250 ₸ = 750 ₸
		fmt.Fprintf(&b, "• %s — %.2f %s × %d ₸ = %d ₸\n",
			it.Name, it.Qty, it.Unit, it.Price, lineAmount)
	}

	// если на сервере есть своя логика наценок/скидок — доверяем аргументу total
	if calcTotal == 0 && total > 0 {
		calcTotal = total
	}

	fmt.Fprintf(&b, "\n💰 Итого к оплате: %d ₸\n", calcTotal)

	// 4) Ссылка Kaspi Pay
	kaspiURL := h.cfg.KaspiPayURL
	if strings.TrimSpace(kaspiURL) == "" {
		kaspiURL = "https://pay.kaspi.kz/pay/e96vsxbs"
	}

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "💳 Оплатить в Kaspi", URL: kaspiURL},
			},
		},
	}

	// 5) Отправка сообщения пользователю
	_, err = h.bot.SendMessage(h.ctx, &bot.SendMessageParams{
		ChatID:      tgid,
		Text:        b.String(),
		ReplyMarkup: kb,
		// ParseMode:   models.ParseModeMarkdown, // если захотите оформление
	})
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
		_, _ = h.bot.SendMessage(h.ctx, &bot.SendMessageParams{
			ChatID: h.cfg.AdminID,
			Text:   text,
		})
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
