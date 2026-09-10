package security

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"wspoc-go/internal/config"
	"wspoc-go/internal/protocol"
)

// ErrInvalidSession se devuelve al pedir un token para un identificador que el
// handshake rechazaría después.
var ErrInvalidSession = errors.New("identificador de sesión inválido")

// Issuer emite los tokens del intercambio SESION -> JWT.
//
// Vive junto al Guard a propósito: emisión y verificación comparten secreto,
// emisor y audiencia, y separarlas en paquetes distintos es lo que permitiría que
// derivaran hasta emitir tokens que el propio servicio rechaza.
type Issuer struct {
	cfg config.Config
}

// NewIssuer crea el emisor con la configuración del proceso.
func NewIssuer(cfg config.Config) Issuer { return Issuer{cfg: cfg} }

// ForSession firma un token HS256 para una sesión y devuelve también su vigencia.
//
// La sesión se escribe en `sid`, en `SESION` y en `sub`: los tres claims que
// CheckToken sabe leer. Emitir los tres hace que el token valga igual si mañana
// lo verifica otro backend con un orden de preferencia distinto.
func (i Issuer) ForSession(session string) (string, time.Duration, error) {
	if !protocol.ValidSessionID(session) {
		return "", 0, ErrInvalidSession
	}

	ttl := time.Duration(i.cfg.SessionTokenTTLSeconds) * time.Second
	now := time.Now()

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   session,
			Issuer:    i.cfg.JWTIssuer,
			Audience:  jwt.ClaimStrings{i.cfg.JWTAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Sid:    session,
		Sesion: session,
	})

	raw, err := token.SignedString([]byte(i.cfg.JWTSecret))
	if err != nil {
		return "", 0, err
	}
	return raw, ttl, nil
}
