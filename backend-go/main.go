// Backend WebSocket del POC — Go, net/http + gorilla/websocket.
//
// Implementa el mismo contrato de seguridad que los backends .NET 10, Java y
// Python (ver SECURITY-CHECKLIST.md).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var cfg PocConfig
var registry *SessionRegistry

func main() {
	cfg = LoadConfig()
	registry = NewSessionRegistry(cfg.MaxConnections)
	outbox = NewOutbox(cfg.OutboxSize)
	pushLimiter = NewSlidingWindowLimiter(cfg.PushRateLimitPerSec)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	SetupDelivery()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/ws", handleWS)
	mux.HandleFunc("/api/push", handlePush)
	mux.HandleFunc("/api/outbox", handleOutbox)

	origins := make([]string, 0, len(cfg.AllowedOrigins))
	for o := range cfg.AllowedOrigins {
		origins = append(origins, o)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.BindAddress, cfg.Port),
		Handler: mux,
		// Cabeceras lentas (slowloris) no deben poder ocupar conexiones indefinidamente.
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf(
		"WebSocket POC (Go) instancia=%s escuchando en ws://%s:%d/ws — máx %d conexiones, %d msg/s por conexión, "+
			"mensajes <= %d bytes, idle %ds, orígenes: %s",
		cfg.InstanceID, cfg.BindAddress, cfg.Port, cfg.MaxConnections, cfg.RateLimitPerSec, cfg.MaxMessageBytes,
		cfg.IdleTimeoutSeconds, strings.Join(origins, ", "),
	)

	// Apagado ordenado: las conexiones vivas se cierran sin dejar el puerto colgado.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("no se pudo levantar el servidor: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"stack":             "go",
		"status":            "ok",
		"instance":          cfg.InstanceID,
		"activeConnections": registry.Active(),
		"maxConnections":    cfg.MaxConnections,
	})
}

// evaluate aplica los controles pre-upgrade en orden: origen -> token -> capacidad.
func evaluate(r *http.Request) Verdict {
	verdict := CheckOrigin(r.Header.Get("Origin"), cfg)
	if !verdict.Allowed {
		return verdict
	}
	verdict = CheckToken(r.URL.Query().Get("token"), cfg)
	if !verdict.Allowed {
		return verdict
	}
	if registry.Active() >= cfg.MaxConnections {
		return deny("SERVER_AT_CAPACITY", 4013, http.StatusServiceUnavailable)
	}
	return verdict
}

var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
	ReadBufferSize:   8192,
	WriteBufferSize:  8192,
	// El Origin ya se validó en evaluate(); gorilla no debe volver a decidir.
	CheckOrigin: func(*http.Request) bool { return true },
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "-"
	}
	remote := r.RemoteAddr
	verdict := evaluate(r)

	// Petición GET normal (sin upgrade): endpoint de diagnóstico del POC.
	if !websocket.IsWebSocketUpgrade(r) {
		setDiagnosticCORS(w, r)
		if verdict.Allowed {
			writeJSON(w, http.StatusOK, map[string]any{"allowed": true, "reason": nil, "code": nil})
			return
		}
		writeJSON(w, verdict.HTTPStatus, map[string]any{
			"allowed": false, "reason": verdict.Reason, "code": verdict.Code,
		})
		return
	}

	if !verdict.Allowed {
		// Rechazo ANTES del upgrade: nunca se abre el WebSocket.
		log.Printf("[REJECT] stack=go reason=%s code=%d remote=%s origin=%s detail=handshake",
			verdict.Reason, verdict.Code, remote, origin)
		http.Error(w, verdict.Reason, verdict.HTTPStatus)
		return
	}

	// Reserva atómica de la plaza antes del upgrade: sin esto, dos handshakes
	// simultáneos podrían pasar la comprobación y superar el máximo.
	active, ok := registry.TryAdd()
	if !ok {
		log.Printf("[REJECT] stack=go reason=SERVER_AT_CAPACITY code=4013 remote=%s origin=%s detail=max=%d",
			remote, origin, cfg.MaxConnections)
		http.Error(w, "SERVER_AT_CAPACITY", http.StatusServiceUnavailable)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		registry.Release()
		log.Printf("[REJECT] stack=go reason=UPGRADE_FAILED code=1002 remote=%s origin=%s detail=%v",
			remote, origin, err)
		return
	}

	connID := uuid.NewString()[:8]
	client := NewClient(conn, verdict.Session, connID)
	registry.Bind(client)
	// Una única goroutine escribe en el socket; el pump y el REST solo encolan.
	go client.writePump()

	log.Printf("[ACCEPT] stack=go conn=%s remote=%s sub=%s sesion=%s active=%d",
		connID, remote, verdict.Subject, verdict.Session, active)

	// Aislamiento de fallos: un pánico en una conexión no debe tumbar el proceso (§7).
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[REJECT] stack=go reason=CONNECTION_FAULT code=1011 remote=%s origin=%s detail=%v",
				remote, origin, rec)
		}
		_ = conn.Close()
		log.Printf("[CLOSE] stack=go conn=%s active=%d", connID, registry.Remove(client))
	}()

	pump(client, remote)
}

// pump es el bucle de la conexión: idle timeout, tamaño, rate limit y eco validado.
func pump(client *Client, remote string) {
	conn, connID := client.conn, client.connID
	limiter := NewSlidingWindowLimiter(cfg.RateLimitPerSec)
	idle := time.Duration(cfg.IdleTimeoutSeconds) * time.Second

	// Tope duro por si el corte en streaming fallase; el límite efectivo es el de abajo.
	conn.SetReadLimit(cfg.MaxMessageBytes * 4)

	for {
		// Idle timeout: la fecha límite NO se renueva con los pong, solo con datos reales (§7).
		if err := conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
			return
		}

		messageType, reader, err := conn.NextReader()
		if err != nil {
			if isTimeout(err) {
				log.Printf("[REJECT] stack=go reason=IDLE_TIMEOUT code=4014 remote=%s origin=- detail=conn=%s", remote, connID)
				closeWith(client, 4014, "IDLE_TIMEOUT")
			}
			return
		}
		if messageType != websocket.TextMessage {
			log.Printf("[REJECT] stack=go reason=INVALID_PAYLOAD code=4010 remote=%s origin=- detail=conn=%s frame=binario", remote, connID)
			closeWith(client, 4010, "INVALID_PAYLOAD")
			return
		}

		// Corte en streaming: se lee un byte más que el máximo; si llega, el frame
		// se descarta sin haberlo materializado entero en memoria (§5).
		raw, err := io.ReadAll(io.LimitReader(reader, cfg.MaxMessageBytes+1))
		if err != nil {
			return
		}
		if int64(len(raw)) > cfg.MaxMessageBytes {
			log.Printf("[REJECT] stack=go reason=MESSAGE_TOO_LARGE code=4009 remote=%s origin=- detail=conn=%s", remote, connID)
			closeWith(client, 4009, "MESSAGE_TOO_LARGE")
			return
		}

		if !limiter.Allow() {
			log.Printf("[REJECT] stack=go reason=RATE_LIMIT_EXCEEDED code=4008 remote=%s origin=- detail=conn=%s limit=%d/s",
				remote, connID, cfg.RateLimitPerSec)
			closeWith(client, 4008, "RATE_LIMIT_EXCEEDED")
			return
		}

		msg, reason := ValidateMessage(raw, cfg)
		if msg == nil {
			log.Printf("[REJECT] stack=go reason=%s code=4010 remote=%s origin=- detail=conn=%s", reason, remote, connID)
			client.Enqueue(frame("error", "-", "", reason, client.session))
			closeWith(client, 4010, reason)
			return
		}

		if msg.Type == "ping" {
			client.Enqueue(frame("pong", msg.ID, "", "", client.session))
			continue
		}

		client.Enqueue(frame("echo", msg.ID, msg.Payload, "", client.session))
		// El mensaje del cliente viaja al .NET 4.8 en el sobre {sesion, payload}.
		outbox.Add(Envelope{Session: client.session, Payload: msg.Payload})
	}
}

// frame construye un mensaje servidor->cliente. `sesion` e `instance` son campos
// extra legítimos: el esquema estricto del §4 solo rige en sentido cliente->servidor.
func frame(kind, id, payload, reason, session string) map[string]any {
	f := map[string]any{
		"type": kind, "id": id, "ts": nowMS(),
		"payload": payload, "instance": cfg.InstanceID, "sesion": session,
	}
	if reason != "" {
		f["reason"] = reason
	}
	return f
}

func closeWith(client *Client, code int, reason string) {
	conn := client.conn
	// Se para el escritor y se ESPERA a que salga antes de tocar el socket: si no,
	// dos goroutines escribirían a la vez y gorilla/websocket no lo admite.
	client.StopWriter()
	// Lo que quedara encolado (p. ej. el frame de `error` de un 4010) se escribe
	// ahora; si no, pararíamos al escritor justo antes de que saliera.
	client.FlushPending()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason)); err != nil {
		return
	}
	// Cerrar el TCP inmediatamente después de escribir el frame descarta lo que aún
	// no ha salido: el cliente vería un 1006 en vez del código real. Se drena la
	// conexión brevemente para completar el cierre ordenado.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	for {
		if _, _, err := conn.NextReader(); err != nil {
			return
		}
	}
}

// setDiagnosticCORS permite que el frontend, que vive en otro puerto y por tanto en otro
// origen, pueda leer la respuesta del diagnóstico; sin esta cabecera el navegador la
// bloquea y la UI no puede mostrar el motivo del rechazo. Se devuelve el origen concreto
// y solo si está en la lista blanca — nunca "*", que abriría el endpoint a cualquier web.
func setDiagnosticCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	if _, ok := cfg.AllowedOrigins[origin]; !ok {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
}

func isTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

func nowMS() int64 { return time.Now().UnixMilli() }

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
