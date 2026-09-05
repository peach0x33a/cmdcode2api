package app

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
)

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// randomPassword returns an n-character alphanumeric password drawn from an
// ambiguity-free charset (no 0/O/o, 1/l/I).
func randomPassword(n int) (string, error) {
	const charset = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	pw := make([]byte, n)
	max := big.NewInt(int64(len(charset)))
	for i := range pw {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		pw[i] = charset[idx.Int64()]
	}
	return string(pw), nil
}
