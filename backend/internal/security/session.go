package security

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// AuthCookieName é o nome do cookie HTTP-Only que transporta o JWT.
	AuthCookieName = "auth_token"
	// TokenDuration define a validade do JWT e do cookie de sessão.
	TokenDuration = 24 * time.Hour
)

type tokenClaims struct {
	UserID string `json:"uid"`
	jwt.RegisteredClaims
}

// GenerateToken gera um JWT assinado com expiração de 24 horas contendo o userID.
func GenerateToken(userID string, secretKey []byte) (string, error) {
	if userID == "" {
		return "", errors.New("user id is required")
	}
	if len(secretKey) == 0 {
		return "", errors.New("secret key is required")
	}

	now := time.Now()
	claims := tokenClaims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(TokenDuration)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secretKey)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}

	return signed, nil
}

// ValidateToken valida o JWT e retorna o ID do usuário se estiver íntegro e dentro da validade.
func ValidateToken(tokenString string, secretKey []byte) (string, error) {
	if tokenString == "" {
		return "", errors.New("token is required")
	}
	if len(secretKey) == 0 {
		return "", errors.New("secret key is required")
	}

	parsed, err := jwt.ParseWithClaims(tokenString, &tokenClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return secretKey, nil
	})
	if err != nil {
		return "", fmt.Errorf("parse token: %w", err)
	}

	claims, ok := parsed.Claims.(*tokenClaims)
	if !ok || !parsed.Valid {
		return "", errors.New("invalid token")
	}
	if claims.UserID == "" {
		return "", errors.New("token missing user id")
	}

	return claims.UserID, nil
}

// SetAuthCookie define o cookie HTTP-Only com o JWT e expiração alinhada ao token (24h).
func SetAuthCookie(w http.ResponseWriter, token string) {
	expires := time.Now().Add(TokenDuration)
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(TokenDuration.Seconds()),
		HttpOnly: true,
		Secure:   cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearAuthCookie remove o cookie de autenticação definindo expiração no passado.
func ClearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// ReadAuthCookie extrai o JWT armazenado no cookie de autenticação.
func ReadAuthCookie(r *http.Request) (string, error) {
	cookie, err := r.Cookie(AuthCookieName)
	if err != nil {
		return "", fmt.Errorf("read auth cookie: %w", err)
	}
	if cookie.Value == "" {
		return "", errors.New("auth cookie is empty")
	}
	return cookie.Value, nil
}

func cookieSecure() bool {
	// Render expõe HTTPS; em dev local use COOKIE_SECURE=false.
	return os.Getenv("COOKIE_SECURE") != "false"
}
