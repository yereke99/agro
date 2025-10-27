package database

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/mattn/go-sqlite3"
)

// InitDatabase initializes the SQLite database
func InitDatabase(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Create tables
	if err := CreateTables(db); err != nil {
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	log.Println("Database initialized successfully")
	return db, nil
}

// CreateTables creates all necessary tables for AGRO club
func CreateTables(db *sql.DB) error {
	tables := []struct {
		name string
		fn   func(*sql.DB) error
	}{
		{"just", createJustTable},     // уже есть (регистрация пользователей)
		{"users", createUsersTable},   // без гео
		{"stores", createStoresTable}, // магазины
		{"categories", createCategoriesTable},
		{"products", createProductsTable},
		{"price_feed", createPriceFeedTable},
		{"subscriptions", createSubscriptionsTable},
		{"orders", createOrdersTable},
		{"order_items", createOrderItemsTable},
	}

	for _, t := range tables {
		if err := t.fn(db); err != nil {
			return fmt.Errorf("create %s table: %w", t.name, err)
		}
	}
	log.Println("All tables created successfully")
	return nil
}

// createJustTable creates the just table (existing)
func createJustTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS just (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		id_user BIGINT NOT NULL UNIQUE,
		userName VARCHAR(255) NOT NULL,
		dataRegistred VARCHAR(50) NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err := db.Exec(stmt)
	return err
}

// users — убраны latitude/longitude и пр. лишнее
func createUsersTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS users (
		id             TEXT PRIMARY KEY,
		user_id        INTEGER NOT NULL UNIQUE,   -- Telegram ID
		nickname       TEXT NOT NULL,
		phone          TEXT,                      -- телефон/Kaspi
		sub_status     TEXT DEFAULT 'inactive',   -- inactive | active | blocked
		sub_until      DATETIME,                  -- дата окончания подписки
		selected_store TEXT,                      -- код магазина
		created_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at     DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_users_user_id ON users(user_id);
	CREATE INDEX IF NOT EXISTS idx_users_sub ON users(sub_status, sub_until);
	CREATE TRIGGER IF NOT EXISTS trg_users_updated_at
	AFTER UPDATE ON users
	FOR EACH ROW BEGIN
	  UPDATE users SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
	END;
	`
	_, err := db.Exec(stmt)
	return err
}

func createStoresTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS stores (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,     -- например: samal3, aksai ...
		name TEXT NOT NULL,            -- Самал-3
		address TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TRIGGER IF NOT EXISTS trg_stores_updated_at
	AFTER UPDATE ON stores
	FOR EACH ROW BEGIN
	  UPDATE stores SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
	END;
	`
	_, err := db.Exec(stmt)
	return err
}

func createCategoriesTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS categories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		slug TEXT NOT NULL UNIQUE,       -- vegetables, fruits, greens, promo
		sort_order INTEGER DEFAULT 0
	);
	`
	_, err := db.Exec(stmt)
	return err
}

func createProductsTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS products (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		emoji TEXT,
		category_slug TEXT NOT NULL,        -- FK (логическая)
		unit TEXT NOT NULL DEFAULT '₸/кг',
		price INTEGER NOT NULL,             -- базовая цена (для подписчиков)
		active INTEGER NOT NULL DEFAULT 1,  -- 1/0
		description TEXT,
		photo_path TEXT,
		store_code TEXT,                    -- 🔹 новая колонка: код точки из stores.code
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_products_cat ON products(category_slug, active);
	CREATE INDEX IF NOT EXISTS idx_products_store ON products(store_code);
	CREATE TRIGGER IF NOT EXISTS trg_products_updated_at
	AFTER UPDATE ON products
	FOR EACH ROW BEGIN
	  UPDATE products SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
	END;
	`
	_, err := db.Exec(stmt)
	return err
}

// Исторический фид цен (по желанию можно не использовать)
func createPriceFeedTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS price_feed (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		product_id INTEGER NOT NULL,
		market TEXT NOT NULL DEFAULT 'Алтын Орда',
		price INTEGER NOT NULL,
		price_date DATE NOT NULL DEFAULT (DATE('now')),
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_price_feed_prod ON price_feed(product_id, price_date);
	`
	_, err := db.Exec(stmt)
	return err
}

func createSubscriptionsTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS subscriptions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,       -- Telegram ID (users.user_id)
		phone TEXT,
		status TEXT NOT NULL DEFAULT 'pending',  -- pending | active | expired | cancelled
		invoice_no TEXT,
		amount INTEGER NOT NULL DEFAULT 3000,
		paid_at DATETIME,
		valid_until DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_sub_user ON subscriptions(user_id, status);
	`
	_, err := db.Exec(stmt)
	return err
}

func createOrdersTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,        -- Telegram ID
		store_code TEXT,                 -- откуда собирать
		total_amount INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'new',  -- new | checking | invoiced | paid | preparing | done | cancelled
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_orders_user ON orders(user_id, created_at);
	CREATE TRIGGER IF NOT EXISTS trg_orders_updated_at
	AFTER UPDATE ON orders
	FOR EACH ROW BEGIN
	  UPDATE orders SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
	END;
	`
	_, err := db.Exec(stmt)
	return err
}

func createOrderItemsTable(db *sql.DB) error {
	const stmt = `
	CREATE TABLE IF NOT EXISTS order_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id INTEGER NOT NULL,
		product_id INTEGER NOT NULL,
		name TEXT NOT NULL,         -- денормализация для удобства
		unit TEXT NOT NULL,
		qty REAL NOT NULL,
		price INTEGER NOT NULL,     -- применённая цена на момент заказа
		amount INTEGER NOT NULL,    -- price * qty (округление по правилам)
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_order_items_order ON order_items(order_id);
	`
	_, err := db.Exec(stmt)
	return err
}
