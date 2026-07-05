package csrf

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

const CookieName = "csrf_token"
const FormField = "_csrf"

func NewToken(secret string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	value := hex.EncodeToString(b)
	return value + ":" + sign(value, secret)
}

func Cookie(token string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func Validate(cookieValue, formValue, secret string) bool {
	if cookieValue == "" || formValue == "" || cookieValue != formValue {
		return false
	}
	parts := strings.SplitN(formValue, ":", 2)
	if len(parts) != 2 {
		return false
	}
	return hmac.Equal([]byte(parts[1]), []byte(sign(parts[0], secret)))
}

func sign(value, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}
