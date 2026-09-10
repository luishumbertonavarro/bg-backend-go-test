// Command server arranca el backend WebSocket del POC.
//
// El servicio mantiene el canal con los navegadores y hace de puente con el
// backend .NET 4.8, que es donde vive la lógica de negocio: le reenvía por REST
// lo que pregunta el cliente y le devuelve la respuesta por el mismo canal.
//
// Este fichero es el ÚNICO sitio donde se construyen las dependencias y se
// deciden sus implementaciones concretas. Todo lo demás las recibe ya resueltas,
// que es lo que permite que ninguna capa alcance estado global.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"wspoc-go/internal/bridge"
	"wspoc-go/internal/config"
	"wspoc-go/internal/logging"
	"wspoc-go/internal/session"
	"wspoc-go/internal/transport/httpapi"
)

const (
	// readHeaderTimeout evita que unas cabeceras lentas (slowloris) ocupen una
	// conexión indefinidamente.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout es cuánto se espera a que las conexiones vivas se cierren.
	shutdownTimeout = 5 * time.Second
)

func main() {
	logging.Setup()

	cfg, err := config.Load()
	if err != nil {
		logging.Fatalf("configuración inválida: %v", err)
	}

	registry := session.NewRegistry(cfg.MaxConnections)
	api := httpapi.New(httpapi.Deps{
		Config:   cfg,
		Registry: registry,
		Bridge:   bridge.New(cfg),
	})

	server := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           api.Routes(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	logStartup(cfg)
	go shutdownOnSignal(server)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logging.Fatalf("no se pudo levantar el servidor: %v", err)
	}
}

// logStartup deja en el arranque los límites efectivos.
//
// No es adorno: cuando una conexión se rechaza en producción, lo primero que hay
// que saber es con qué configuración está corriendo esta réplica.
func logStartup(cfg config.Config) {
	// La procedencia va en su propia línea y la primera: es lo que responde
	// "¿con qué secreto está firmando esto?" sin tener que adivinarlo.
	origen := cfg.DotEnvPath
	if origen == "" {
		origen = "(ningún .env encontrado; solo variables de entorno)"
	}
	logging.Infof("configuración cargada de %s", origen)

	logging.Infof(
		"WebSocket POC (Go) instancia=%s escuchando en ws://%s:%d/ws — máx %d conexiones, %d msg/s por conexión, "+
			"mensajes <= %d bytes, idle %ds, orígenes: %s",
		cfg.InstanceID, cfg.BindAddress, cfg.Port, cfg.MaxConnections, cfg.RateLimitPerSec, cfg.MaxMessageBytes,
		cfg.IdleTimeoutSeconds, strings.Join(cfg.OriginList(), ", "),
	)
}

// shutdownOnSignal apaga de forma ordenada: las conexiones vivas se cierran sin
// dejar el puerto colgado.
func shutdownOnSignal(server *http.Server) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = server.Shutdown(ctx)
}
