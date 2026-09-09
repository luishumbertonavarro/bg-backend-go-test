// Package security implementa los controles del handshake: origen y token.
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

// Verdict es el resultado de un control del handshake.
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
//
// La sesión puede venir en tres sitios porque hay dos emisores: `sid` es el
// claim canónico, `SESION` es el nombre con el que la sesión viaja en el resto
// del contrato con el .NET (y el que emite el intercambio del frontend), y `sub`
// es el último recurso para los tokens firmados a mano que ya existían.
type claims struct {
	jwt.RegisteredClaims
	Sid    string `json:"sid,omitempty"`
	Sesion string `json:"SESION,omitempty"`
	// Canal marca los tokens que emite el intercambio del frontend. Solo sirven
	// para abrir el canal; el puente REST los rechaza. Ver CheckBridgeToken.
	Canal bool `json:"canal,omitempty"`
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

// CheckOrigin compara el Origin exacto contra la lista blanca.
//
// Sin cabecera Origin no es un navegador: se permite para que las pruebas de
// carga desde Node/k6 funcionen. En producción esto se endurecería a rechazo.
func (g Guard) CheckOrigin(origin string) Verdict {
	if origin == "" || g.cfg.OriginAllowed(origin) {
		return allowed("", "")
	}
	return denied(protocol.OriginNotAllowed)
}

// CheckToken valida por completo el JWT HS256. Es el control del canal.
func (g Guard) CheckToken(raw string) Verdict {
	verdict, _ := g.parseToken(raw)
	return verdict
}

// CheckBridgeToken valida el token del puente REST (/api/push, /api/outbox).
//
// Es CheckToken más una exclusión: rechaza los tokens que emite el intercambio
// del frontend, que van marcados con `canal`.
//
// Hace falta porque las dos puertas compartían control y el intercambio rompió esa
// simetría: reparte tokens válidos a quien traiga una SESION, y con uno de ellos
// se podía inyectar un push en la sesión de cualquier otro usuario.
//
// Es una exclusión y no un permiso explícito porque el backend que empuja no está
// bajo nuestro control y no puede añadir un claim nuevo a sus tokens. Lo que
// sostiene el control es que para obtener un token SIN la marca hay que conocer
// WS_JWT_SECRET — exactamente el requisito que este endpoint tenía antes de que
// el intercambio existiera. La contrapartida es que un emisor nuevo que olvidara
// la marca volvería a abrir el puente; por eso la marca la pone Issuer y no cada
// punto de emisión.
func (g Guard) CheckBridgeToken(raw string) Verdict {
	verdict, parsed := g.parseToken(raw)
	if !verdict.Allowed {
		return verdict
	}
	if parsed.Canal {
		return denied(protocol.TokenClaimsInvalid)
	}
	return verdict
}

// parseToken hace la verificación común y devuelve también los claims, para que
// quien necesite mirar dentro no tenga que volver a parsear (ni a decidir por su
// cuenta qué hace válido a un token).
func (g Guard) parseToken(raw string) (Verdict, claims) {
	if strings.TrimSpace(raw) == "" {
		return denied(protocol.TokenMissing), claims{}
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
		return g.verdictFromClaims(parsed), parsed
	case errors.Is(err, jwt.ErrTokenExpired):
		return denied(protocol.TokenExpired), claims{}
	case errors.Is(err, jwt.ErrTokenInvalidIssuer),
		errors.Is(err, jwt.ErrTokenInvalidAudience),
		errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return denied(protocol.TokenClaimsInvalid), claims{}
	default:
		return denied(protocol.TokenInvalid), claims{}
	}
}

// verdictFromClaims comprueba lo que la firma por sí sola no garantiza: que el
// token identifique una sesión y que esa sesión sea un identificador utilizable.
//
// Lo que se exige es la SESIÓN, no el `sub`: es la sesión la que enruta los
// mensajes, así que un token sin ella no sirve para nada aunque identifique a
// alguien. `sub` queda para la traza de auditoría y puede venir vacío.
func (g Guard) verdictFromClaims(parsed claims) Verdict {
	// Debe ser un identificador seguro: viaja en URLs y en logs.
	session := firstNonEmpty(parsed.Sid, parsed.Sesion, parsed.Subject)
	if !protocol.ValidSessionID(session) {
		return denied(protocol.TokenClaimsInvalid)
	}
	return allowed(parsed.Subject, session)
}

// firstNonEmpty devuelve el primer valor con contenido, o vacío si no hay ninguno.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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
