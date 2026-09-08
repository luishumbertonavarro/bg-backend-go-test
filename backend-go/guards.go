package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Verdict es el resultado de los controles del handshake (SECURITY-CHECKLIST.md §9).
type Verdict struct {
	Allowed    bool
	Reason     string
	Code       int
	HTTPStatus int
	Subject    string
	// Session identifica la sesión a la que enrutar los mensajes. Sale del token,
	// nunca del cliente: así nadie puede declarar la sesión de otro.
	Session string
}

var verdictOK = Verdict{Allowed: true, HTTPStatus: http.StatusOK}

func deny(reason string, code, status int) Verdict {
	return Verdict{Allowed: false, Reason: reason, Code: code, HTTPStatus: status}
}

// ClientMessage es el único esquema aceptado por el canal.
type ClientMessage struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	TS      int64  `json:"ts"`
	Payload string `json:"payload"`
}

var scriptPatterns = []string{"<script", "javascript:", "onerror=", "onload=", "<iframe", "data:text/html"}

// ------------------------------------------------------------------ handshake

// CheckOrigin compara el Origin exacto contra la lista blanca (§3).
// Sin cabecera Origin no es un navegador: se permite para que las pruebas de carga
// desde Node/k6 funcionen. En producción esto se endurecería a rechazo.
func CheckOrigin(origin string, cfg PocConfig) Verdict {
	if origin == "" {
		return verdictOK
	}
	if _, ok := cfg.AllowedOrigins[origin]; ok {
		return verdictOK
	}
	return deny("ORIGIN_NOT_ALLOWED", 4403, http.StatusForbidden)
}

// pocClaims son los claims registrados más el identificador de sesión del POC.
// `sid` es opcional: un token sin él usa `sub` como sesión.
type pocClaims struct {
	jwt.RegisteredClaims
	Sid string `json:"sid,omitempty"`
}

// CheckToken valida por completo el JWT HS256 (§2).
func CheckToken(raw string, cfg PocConfig) Verdict {
	if strings.TrimSpace(raw) == "" {
		return deny("TOKEN_MISSING", 4001, http.StatusUnauthorized)
	}

	claims := pocClaims{}
	_, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
		return []byte(cfg.JWTSecret), nil
	},
		// Algoritmo fijo: cierra el ataque de confusión de algoritmo ("alg": "none").
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(cfg.JWTIssuer),
		jwt.WithAudience(cfg.JWTAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(0),
	)

	switch {
	case err == nil:
		if claims.Subject == "" {
			return deny("TOKEN_CLAIMS_INVALID", 4004, http.StatusUnauthorized)
		}
		// La sesión sale del claim `sid`; sin él, del `sub`. Debe ser un
		// identificador seguro: viaja en URLs y en logs.
		session := claims.Sid
		if session == "" {
			session = claims.Subject
		}
		if len(session) > 64 || !isSafeID(session) {
			return deny("TOKEN_CLAIMS_INVALID", 4004, http.StatusUnauthorized)
		}
		v := verdictOK
		v.Subject = claims.Subject
		v.Session = session
		return v
	case errors.Is(err, jwt.ErrTokenExpired):
		return deny("TOKEN_EXPIRED", 4003, http.StatusUnauthorized)
	case errors.Is(err, jwt.ErrTokenInvalidIssuer),
		errors.Is(err, jwt.ErrTokenInvalidAudience),
		errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return deny("TOKEN_CLAIMS_INVALID", 4004, http.StatusUnauthorized)
	default:
		return deny("TOKEN_INVALID", 4002, http.StatusUnauthorized)
	}
}

// ------------------------------------------------------------------- mensajes

// ValidateMessage valida y sanea un frame entrante (§4).
func ValidateMessage(raw []byte, cfg PocConfig) (*ClientMessage, string) {
	if int64(len(raw)) > cfg.MaxMessageBytes {
		return nil, "MESSAGE_TOO_LARGE"
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	// Esquema estricto: cualquier campo fuera del contrato hace fallar la decodificación.
	decoder.DisallowUnknownFields()

	var msg ClientMessage
	if err := decoder.Decode(&msg); err != nil {
		return nil, "INVALID_PAYLOAD"
	}
	// Un segundo documento JSON pegado detrás también es inválido.
	if decoder.More() {
		return nil, "INVALID_PAYLOAD"
	}

	if msg.Type != "echo" && msg.Type != "ping" {
		return nil, "INVALID_PAYLOAD"
	}
	if msg.TS == 0 {
		return nil, "INVALID_PAYLOAD"
	}
	if len(msg.ID) == 0 || len(msg.ID) > 64 || !isSafeID(msg.ID) {
		return nil, "INVALID_PAYLOAD"
	}
	if len(msg.Payload) > 8192 {
		return nil, "INVALID_PAYLOAD"
	}
	if hasControlChars(msg.Payload) {
		return nil, "INVALID_PAYLOAD"
	}
	lowered := strings.ToLower(msg.Payload)
	for _, pattern := range scriptPatterns {
		if strings.Contains(lowered, pattern) {
			return nil, "INVALID_PAYLOAD"
		}
	}
	return &msg, ""
}

// ValidateEnvelope valida el sobre {sesion, payload} que manda el .NET 4.8.
// Aplica la misma sanitización que el canal WebSocket: el push acaba en el DOM
// del navegador igual que un eco, así que no puede ser un camino más laxo.
func ValidateEnvelope(raw []byte, cfg PocConfig) (*Envelope, string) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var env Envelope
	if err := decoder.Decode(&env); err != nil {
		return nil, "INVALID_PAYLOAD"
	}
	if decoder.More() {
		return nil, "INVALID_PAYLOAD"
	}

	if len(env.Session) == 0 || len(env.Session) > 64 || !isSafeID(env.Session) {
		return nil, "INVALID_SESSION"
	}
	if len(env.Payload) > 8192 || hasControlChars(env.Payload) {
		return nil, "INVALID_PAYLOAD"
	}
	lowered := strings.ToLower(env.Payload)
	for _, pattern := range scriptPatterns {
		if strings.Contains(lowered, pattern) {
			return nil, "INVALID_PAYLOAD"
		}
	}
	return &env, ""
}

func isSafeID(id string) bool {
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func hasControlChars(s string) bool {
	for _, c := range s {
		if c < 32 && c != '\t' && c != '\n' && c != '\r' {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------- rate limit

// SlidingWindowLimiter aplica el rate limit por conexión (§6). No necesita mutex:
// cada conexión tiene el suyo y solo lo usa su propia goroutine de lectura.
type SlidingWindowLimiter struct {
	max    int
	stamps []time.Time
}

func NewSlidingWindowLimiter(maxPerSecond int) *SlidingWindowLimiter {
	return &SlidingWindowLimiter{max: maxPerSecond, stamps: make([]time.Time, 0, maxPerSecond)}
}

func (l *SlidingWindowLimiter) Allow() bool {
	now := time.Now()
	cutoff := now.Add(-time.Second)
	kept := l.stamps[:0]
	for _, t := range l.stamps {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.stamps = kept
	if len(l.stamps) >= l.max {
		return false
	}
	l.stamps = append(l.stamps, now)
	return true
}

// El registro de conexiones vive ahora en sessions.go (SessionRegistry): además
// del contador del tope anti-DoS (§7), guarda el mapa de sesión -> conexiones que
// hace posible entregar a un usuario concreto.
