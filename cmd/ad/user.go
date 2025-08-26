package main

import (
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID           int
	TeamID       int
	Username     string
	PasswordHash string
}

func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), 14)
	return string(bytes), err
}

func (user *User) VerifyPassword(password string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password))
	return err == nil
}
