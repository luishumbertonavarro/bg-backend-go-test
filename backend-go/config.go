package main

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PocConfig es la configuración compartida por los cuatro backends
// (SECURITY-CHECKLIST.md §0). Se lee del .env de la raíz del repositorio y las
// variables de entorno reales tienen prioridad.
type PocConfig struct {
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

// loadDotEnv busca el .env subiendo desde el directorio actual hasta la raíz.
func loadDotEnv() map[string]string {
	values := map[string]string{}
	dir, err := os.Getwd()
	if err != nil {
		return values
	}
	for {
		candidate := filepath.Join(dir, ".env")
		if file, err := os.Open(candidate); err == nil {
			defer file.Close()
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
			return values
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return values
		}
		dir = parent
	}
}

func LoadConfig() PocConfig {
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
	if len(secret) < 32 {
		log.Fatal("WS_JWT_SECRET ausente o menor a 32 bytes. Revisa el .env de la raíz.")
	}

	origins := map[string]struct{}{}
	for _, o := range strings.Split(get("WS_ALLOWED_ORIGINS", "http://localhost:4200"), ",") {
		if trimmed := strings.TrimSpace(o); trimmed != "" {
			origins[trimmed] = struct{}{}
		}
	}

	// El default es loopback a proposito: solo un despliegue que lo pida
	// explicitamente (los manifiestos de k8s) queda expuesto en todas las interfaces.
	bind := get("WS_BIND_ADDRESS", "127.0.0.1")

	// Identifica la replica que atiende. En k8s se inyecta el nombre del pod;
	// fuera de k8s el hostname ya distingue maquinas.
	instance := get("WS_INSTANCE_ID", "")
	if instance == "" {
		if h, err := os.Hostname(); err == nil {
			instance = h
		} else {
			instance = "desconocida"
		}
	}

	return PocConfig{
		Port:               getInt("WS_PORT", getInt("WS_PORT_GO", 8084)),
		BindAddress:        bind,
		InstanceID:         instance,
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
	}
}
