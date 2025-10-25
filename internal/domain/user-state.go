package domain

import "time"

// JustEntry represents a user registration in the just table
type JustEntry struct {
	Id             int64  `json:"id" db:"id"`
	UserId         int64  `json:"userID" db:"id_user"`
	UserName       string `json:"userName" db:"userName"`
	DateRegistered string `json:"dateRegistered" db:"dataRegistred"`
}

type User struct {
	Id         string
	TelegramId int64
	Nickname   string
	Sex        string
	Age        int
	Latitude   *float64
	Longitude  *float64
	AboutUser  string
	AvatarPath string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type UserState struct {
	State         string `json:"state"`
	BroadCastType string `json:"broadcast_type"`
	Count         int    `json:"count"`
	Contact       string `json:"contact"`
	IsPaid        bool   `json:"is_paid"`
}
