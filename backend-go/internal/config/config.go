// Package config carga la configuración compartida por los cuatro backends del
// POC (SECURITY-CHECKLIST.md §0).
//
// No depende de ningún otro paquete del proyecto: es la hoja del grafo de
// dependencias, así que cualquier capa puede importarlo sin crear ciclos.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config es la configuración efectiva del proceso.
type Config struct {
	Port               int
	BindAddress        string
	InstanceID         string
	JWTSecret          string
	JWTIssuer          string
	JWTAudience        string
	AllowedOrigins     map[string]struct{}
	MaxMessageBytes    int64
	RateLimitPerSec    int
	MaxConnections     int32
	IdleTimeoutSeconds int

	// Puente con el backend .NET 4.8.
	DotNetWebhookURL     string // vacío = modo outbox inspeccionable
	DotNetTimeoutSeconds int
	PushRateLimitPerSec  int
	OutboxSize           int
	RedisAddr            string // vacío = entrega local, una réplica
}

// minSecretBytes es la longitud mínima del secreto HS256. Por debajo de 32 bytes
// la firma es forzable por fuerza bruta y el control del §2 dejaría de sostenerse.
const minSecretBytes = 32

// Load lee el .env y las variables de entorno, que tienen prioridad.
//
// Devuelve error en vez de abortar el proceso: quién decide morir es main, no una
// biblioteca. Eso además permite probar la carga sin matar el binario de test.
func Load() (Config, error) {
	env := loadDotEnv()

	get := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		if v, ok := env[key]; ok && v != "" {
			return v
		}
		return fallback
	}
	getInt := func(key string, fallback int) int {
		if n, err := strconv.Atoi(get(key, strconv.Itoa(fallback))); err == nil {
			return n
		}
		return fallback
	}

	secret := get("WS_JWT_SECRET", "")
	if len(secret) < minSecretBytes {
		return Config{}, fmt.Errorf(
			"WS_JWT_SECRET ausente o menor a %d bytes. Revisa el .env de la raíz", minSecretBytes)
	}

	origins := map[string]struct{}{}
	for _, o := range strings.Split(get("WS_ALLOWED_ORIGINS", "http://localhost:4200"), ",") {
		if trimmed := strings.TrimSpace(o); trimmed != "" {
			origins[trimmed] = struct{}{}
		}
	}

	return Config{
		// El default es loopback a propósito: solo un despliegue que lo pida
		// explícitamente (los manifiestos de k8s) queda expuesto en todas las interfaces.
		BindAddress:        get("WS_BIND_ADDRESS", "127.0.0.1"),
		Port:               getInt("WS_PORT", getInt("WS_PORT_GO", 8084)),
		InstanceID:         instanceID(get("WS_INSTANCE_ID", "")),
		JWTSecret:          secret,
		JWTIssuer:          get("WS_JWT_ISSUER", "ws-poc-issuer"),
		JWTAudience:        get("WS_JWT_AUDIENCE", "ws-poc-clients"),
		AllowedOrigins:     origins,
		MaxMessageBytes:    int64(getInt("WS_MAX_MESSAGE_BYTES", 65536)),
		RateLimitPerSec:    getInt("WS_RATE_LIMIT_PER_SEC", 20),
		MaxConnections:     int32(getInt("WS_MAX_CONNECTIONS", 200)),
		IdleTimeoutSeconds: getInt("WS_IDLE_TIMEOUT_SECONDS", 60),

		DotNetWebhookURL:     get("WS_DOTNET_WEBHOOK_URL", ""),
		DotNetTimeoutSeconds: getInt("WS_DOTNET_TIMEOUT_SECONDS", 5),
		PushRateLimitPerSec:  getInt("WS_PUSH_RATE_LIMIT_PER_SEC", 100),
		OutboxSize:           getInt("WS_OUTBOX_SIZE", 200),
		RedisAddr:            get("WS_REDIS_ADDR", ""),
	}, nil
}

// OriginAllowed indica si el origen está en la lista blanca.
//
// Existe para que nadie fuera de este paquete tenga que conocer que la lista es
// un map[string]struct{}: si mañana pasa a ser una lista de patrones, solo cambia aquí.
func (c Config) OriginAllowed(origin string) bool {
	_, ok := c.AllowedOrigins[origin]
	return ok
}

// OriginList devuelve los orígenes permitidos, para el log de arranque.
func (c Config) OriginList() []string {
	origins := make([]string, 0, len(c.AllowedOrigins))
	for o := range c.AllowedOrigins {
		origins = append(origins, o)
	}
	return origins
}

// Addr es la dirección de escucha del servidor.
func (c Config) Addr() string { return fmt.Sprintf("%s:%d", c.BindAddress, c.Port) }

// instanceID identifica la réplica que atiende. En k8s se inyecta el nombre del
// pod; fuera de k8s el hostname ya distingue máquinas.
func instanceID(configured string) string {
	if configured != "" {
		return configured
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "desconocida"
}

// loadDotEnv busca el .env subiendo desde el directorio actual hasta la raíz.
func loadDotEnv() map[string]string {
	values := map[string]string{}
	dir, err := os.Getwd()
	if err != nil {
		return values
	}
	for {
		if parsed, ok := parseDotEnv(filepath.Join(dir, ".env")); ok {
			return parsed
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return values
		}
		dir = parent
	}
}

// parseDotEnv lee un fichero .env. El bool distingue "no existe" de "existe y
// está vacío", que es lo que decide si loadDotEnv sigue subiendo directorios.
func parseDotEnv(path string) (map[string]string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values, true
}
