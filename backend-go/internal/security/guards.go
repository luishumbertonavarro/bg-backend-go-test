// Package security implementa los controles del handshake: origen (§3) y token (§2).
//
// Ninguna función de aquí escribe en la respuesta HTTP ni cierra sockets: solo
// emite un veredicto. Quién lo traduce a un 401, a un frame de cierre 4001 o a
// una línea de log es cosa de la capa de transporte. Esa separación es la que
// permite que el mismo control valga para el handshake del WebSocket y para la
// cabecera Authorization del REST.
package security

import (
	"errors"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"wspoc-go/internal/config"
	"wspoc-go/internal/protocol"
)

// Verdict es el resultado de un control del handshake (§9).
type Verdict struct {
	Allowed bool
	// Rejection solo tiene sentido cuando Allowed es false.
	Rejection protocol.Rejection
	// Subject es el `sub` del token, para la traza de auditoría.
	Subject string
	// Session identifica la sesión a la que enrutar los mensajes. Sale del token,
	// nunca del cliente: así nadie puede declarar la sesión de otro.
	Session string
}

// allowed construye un veredicto favorable.
func allowed(subject, session string) Verdict {
	return Verdict{Allowed: true, Subject: subject, Session: session}
}

// denied construye un veredicto de rechazo a partir del catálogo del protocolo.
func denied(rejection protocol.Rejection) Verdict {
	return Verdict{Allowed: false, Rejection: rejection}
}

// claims son los registrados más el identificador de sesión del POC.
// `sid` es opcional: un token sin él usa `sub` como sesión.
type claims struct {
	jwt.RegisteredClaims
	Sid string `json:"sid,omitempty"`
}

// Guard aplica los controles con una configuración concreta.
//
// Recibe la configuración al construirse en lugar de alcanzar una variable
// global: es lo que hace que estos controles se puedan probar con distintos
// secretos y listas de orígenes sin tocar el estado del proceso.
type Guard struct {
	cfg config.Config
}

// NewGuard crea el evaluador de controles del handshake.
func NewGuard(cfg config.Config) Guard { return Guard{cfg: cfg} }

// CheckOrigin compara el Origin exacto contra la lista blanca (§3).
//
// Sin cabecera Origin no es un navegador: se permite para que las pruebas de
// carga desde Node/k6 funcionen. En producción esto se endurecería a rechazo.
func (g Guard) CheckOrigin(origin string) Verdict {
	if origin == "" || g.cfg.OriginAllowed(origin) {
		return allowed("", "")
	}
	return denied(protocol.OriginNotAllowed)
}

// CheckToken valida por completo el JWT HS256 (§2).
func (g Guard) CheckToken(raw string) Verdict {
	if strings.TrimSpace(raw) == "" {
		return denied(protocol.TokenMissing)
	}

	parsed := claims{}
	_, err := jwt.ParseWithClaims(raw, &parsed, func(*jwt.Token) (any, error) {
		return []byte(g.cfg.JWTSecret), nil
	},
		// Algoritmo fijo: cierra el ataque de confusión de algoritmo ("alg": "none").
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(g.cfg.JWTIssuer),
		jwt.WithAudience(g.cfg.JWTAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(0),
	)

	switch {
	case err == nil:
		return g.verdictFromClaims(parsed)
	case errors.Is(err, jwt.ErrTokenExpired):
		return denied(protocol.TokenExpired)
	case errors.Is(err, jwt.ErrTokenInvalidIssuer),
		errors.Is(err, jwt.ErrTokenInvalidAudience),
		errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return denied(protocol.TokenClaimsInvalid)
	default:
		return denied(protocol.TokenInvalid)
	}
}

// verdictFromClaims comprueba lo que la firma por sí sola no garantiza: que el
// token identifique a alguien y que su sesión sea un identificador utilizable.
func (g Guard) verdictFromClaims(parsed claims) Verdict {
	if parsed.Subject == "" {
		return denied(protocol.TokenClaimsInvalid)
	}
	// La sesión sale del claim `sid`; sin él, del `sub`. Debe ser un
	// identificador seguro: viaja en URLs y en logs.
	session := parsed.Sid
	if session == "" {
		session = parsed.Subject
	}
	if !protocol.ValidSessionID(session) {
		return denied(protocol.TokenClaimsInvalid)
	}
	return allowed(parsed.Subject, session)
}

// BearerToken extrae el token de una cabecera Authorization.
//
// El prefijo "Bearer " es opcional: se acepta también el token pelado, porque el
// .NET 4.8 que llama al puente puede montar la cabecera de cualquiera de las dos formas.
func BearerToken(header string) string {
	if header == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[len("bearer "):])
	}
	return strings.TrimSpace(header)
}
