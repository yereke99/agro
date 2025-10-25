package repository

import (
	"agro/internal/domain"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type UserRepository struct {
	db *sql.DB
}

func NewUserRepository(db *sql.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) GetAllJustUserIDs(ctx context.Context) ([]int64, error) {
	const q = `SELECT id_user FROM just ORDER BY created_at DESC;`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var userIDs []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			continue
		}
		userIDs = append(userIDs, userID)
	}
	return userIDs, nil
}

func (r *UserRepository) UpdateUser(user *domain.User) error {
	if user == nil || user.Id == "" {
		return errors.New("UpdateUser: empty user or user.Id")
	}
	const q = `
		UPDATE users
		SET
			nickname    = ?,
			sex         = ?,
			age         = ?,
			latitude    = ?,
			longitude   = ?,
			about_user  = ?,
			avatar_path = ?,
			updated_at  = CURRENT_TIMESTAMP
		WHERE id = ?
	`

	nullableFloat64 := func(p *float64) interface{} {
		if p == nil {
			return nil
		}
		return *p
	}

	res, err := r.db.Exec(
		q,
		user.Nickname,
		user.Sex,
		user.Age,
		nullableFloat64(user.Latitude),
		nullableFloat64(user.Longitude),
		user.AboutUser,
		user.AvatarPath,
		user.Id,
	)
	if err != nil {
		return fmt.Errorf("UpdateUser exec: %w", err)
	}

	ra, _ := res.RowsAffected()
	if ra == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ExistsJust проверяет, есть ли запись в just по id_user
func (r *UserRepository) ExistsJust(ctx context.Context, userId int64) (bool, error) {
	const q = `SELECT COUNT(1) FROM just WHERE id_user=?;`
	var cnt int
	if err := r.db.QueryRowContext(ctx, q, userId).Scan(&cnt); err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// InsertJust вставляет запись в таблицу just с учетом новых полей (SQLite version)
func (r *UserRepository) InsertJust(ctx context.Context, e domain.JustEntry) error {
	const q = `
		INSERT OR REPLACE INTO just (id_user, userName, dataRegistred, updated_at)
		VALUES (?, ?, ?, datetime('now'));
	`
	_, err := r.db.ExecContext(ctx, q, e.UserId, e.UserName, e.DateRegistered)
	return err
}

// в repository.UserRepository
func (r *UserRepository) GetUserByID(id string) (*domain.User, error) {
	const q = `
		SELECT id, user_id, nickname, sex, age, latitude, longitude, about_user, avatar_path, created_at, updated_at
		FROM users
		WHERE id = ?
		LIMIT 1`
	row := r.db.QueryRow(q, id)

	var u domain.User
	var lat, lon sql.NullFloat64
	if err := row.Scan(&u.Id, &u.TelegramId, &u.Nickname, &u.Sex, &u.Age, &lat, &lon, &u.AboutUser, &u.AvatarPath, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if lat.Valid {
		u.Latitude = &lat.Float64
	}
	if lon.Valid {
		u.Longitude = &lon.Float64
	}
	return &u, nil
}

// Простой поиск без координат (для случая, когда location не пришёл)
func (r *UserRepository) FindUsersByFilters(sex string, ageMin, ageMax *int, q string, limit int) ([]domain.User, error) {
	query := `
		SELECT id, user_id, nickname, sex, age, latitude, longitude, about_user, avatar_path, created_at, updated_at
		FROM users
		WHERE 1=1
	`
	args := []any{}

	if sex != "" {
		query += " AND sex = ?"
		args = append(args, sex)
	}
	if ageMin != nil {
		query += " AND age >= ?"
		args = append(args, *ageMin)
	}
	if ageMax != nil {
		query += " AND age <= ?"
		args = append(args, *ageMax)
	}
	if q != "" {
		query += " AND (LOWER(nickname) LIKE ? OR LOWER(about_user) LIKE ?)"
		pat := "%" + strings.ToLower(q) + "%"
		args = append(args, pat, pat)
	}

	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []domain.User
	for rows.Next() {
		var u domain.User
		var lat, lon sql.NullFloat64
		if err := rows.Scan(&u.Id, &u.TelegramId, &u.Nickname, &u.Sex, &u.Age, &lat, &lon, &u.AboutUser, &u.AvatarPath, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		if lat.Valid {
			u.Latitude = &lat.Float64
		}
		if lon.Valid {
			u.Longitude = &lon.Float64
		}
		res = append(res, u)
	}
	return res, rows.Err()
}

// GetUserNickname возвращает user_nickname для данного user_id.
func (r *UserRepository) GetUserNickname(userID int64) (string, error) {
	query := `SELECT nickname FROM users WHERE user_id = ?`
	var nickname string
	if err := r.db.QueryRow(query, userID).Scan(&nickname); err != nil {
		// Если записи не найдено, можно вернуть пустую строку или ошибку
		return "", fmt.Errorf("GetUserNickname қатесі: %w", err)
	}
	return nickname, nil
}

// Кандидаты по bbox + фильтры
func (r *UserRepository) FindUsersInBBox(latMin, latMax, lonMin, lonMax float64, sex string, ageMin, ageMax *int, q string, limit int) ([]domain.User, error) {
	query := `
		SELECT id, user_id, nickname, sex, age, latitude, longitude, about_user, avatar_path, created_at, updated_at
		FROM users
		WHERE latitude IS NOT NULL AND longitude IS NOT NULL
		  AND latitude BETWEEN ? AND ?
		  AND longitude BETWEEN ? AND ?
	`
	args := []any{latMin, latMax, lonMin, lonMax}

	if sex != "" {
		query += " AND sex = ?"
		args = append(args, sex)
	}
	if ageMin != nil {
		query += " AND age >= ?"
		args = append(args, *ageMin)
	}
	if ageMax != nil {
		query += " AND age <= ?"
		args = append(args, *ageMax)
	}
	if q != "" {
		query += " AND (LOWER(nickname) LIKE ? OR LOWER(about_user) LIKE ?)"
		pat := "%" + strings.ToLower(q) + "%"
		args = append(args, pat, pat)
	}

	// Берём побольше — финальный радиус отфильтруем в Go
	query += " ORDER BY updated_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []domain.User
	for rows.Next() {
		var u domain.User
		var lat, lon sql.NullFloat64
		if err := rows.Scan(&u.Id, &u.TelegramId, &u.Nickname, &u.Sex, &u.Age, &lat, &lon, &u.AboutUser, &u.AvatarPath, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		if lat.Valid {
			u.Latitude = &lat.Float64
		}
		if lon.Valid {
			u.Longitude = &lon.Float64
		}
		res = append(res, u)
	}
	return res, rows.Err()
}

func (r *UserRepository) CheckUserExists(telegramId int64) (bool, error) {
	var exists bool
	query := `SELECT EXISTS(SELECT 1 FROM users WHERE user_id = $1)`
	err := r.db.QueryRow(query, telegramId).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check user existence: %w", err)
	}
	return exists, nil
}

func (r *UserRepository) GetUserByTelegramId(telegramId int64) (*domain.User, error) {
	user := &domain.User{}
	query := `
		SELECT id, user_id, nickname, sex, age, latitude, longitude, 
		       about_user, COALESCE(avatar_path, ''), created_at
		FROM users 
		WHERE user_id = $1
	`
	err := r.db.QueryRow(query, telegramId).Scan(
		&user.Id,
		&user.TelegramId,
		&user.Nickname,
		&user.Sex,
		&user.Age,
		&user.Latitude,
		&user.Longitude,
		&user.AboutUser,
		&user.AvatarPath,
		&user.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	return user, nil
}

func (r *UserRepository) CreateUser(user *domain.User) (string, error) {
	userId := uuid.New().String()

	query := `
		INSERT INTO users (id, user_id, nickname, sex, age, latitude, longitude, about_user, avatar_path)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`

	err := r.db.QueryRow(
		query,
		userId,
		user.TelegramId,
		user.Nickname,
		user.Sex,
		user.Age,
		user.Latitude,
		user.Longitude,
		user.AboutUser,
		user.AvatarPath,
	).Scan(&userId)

	if err != nil {
		return "", fmt.Errorf("failed to create user: %w", err)
	}

	return userId, nil
}

func (r *UserRepository) GetNearbyUsers(location string, limit int) ([]*domain.User, error) {
	query := `
		SELECT id, user_id, nickname, sex, age, latitude, longitude, 
		       about_user, COALESCE(avatar_path, ''), created_at
		FROM users
		ORDER BY created_at DESC
		LIMIT $1
	`

	rows, err := r.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get nearby users: %w", err)
	}
	defer rows.Close()

	var users []*domain.User
	for rows.Next() {
		user := &domain.User{}
		err := rows.Scan(
			&user.Id,
			&user.TelegramId,
			&user.Nickname,
			&user.Sex,
			&user.Age,
			&user.Latitude,
			&user.Longitude,
			&user.AboutUser,
			&user.AvatarPath,
			&user.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan user: %w", err)
		}
		users = append(users, user)
	}

	return users, nil
}
