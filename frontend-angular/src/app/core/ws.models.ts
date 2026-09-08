/**
 * Contrato compartido con el backend Go.
 * Cualquier cambio aquí debe reflejarse en SECURITY-CHECKLIST.md y en los 4 servidores.
 */

export type BackendId = 'go';

export interface BackendDescriptor {
  id: BackendId;
  label: string;
  /** Puerto por defecto según SECURITY-CHECKLIST.md §0 */
  defaultUrl: string;
  hint: string;
}

/** Mensaje que el cliente envía. Esquema estricto: ningún campo extra. */
export interface ClientMessage {
  type: 'echo' | 'ping';
  id: string;
  ts: number;
  payload: string;
}

/** Mensaje que el servidor devuelve. */
export interface ServerMessage {
  /** `push` es un mensaje que el backend .NET 4.8 empuja a esta sesión. */
  type: 'echo' | 'pong' | 'error' | 'push';
  id: string;
  ts: number;
  payload?: string;
  reason?: string;
  /**
   * Identifica la réplica que atendió la conexión (en Kubernetes, el nombre del
   * pod). Opcional: los backends que aún no lo emiten siguen siendo válidos.
   *
   * El esquema estricto de §4 solo rige en sentido cliente -> servidor, así que
   * añadir este campo a la respuesta no afecta a ese control.
   */
  instance?: string;
  /**
   * Sesión a la que pertenece la conexión. Sale del claim `sid` del JWT, no la
   * elige el cliente. Es la dirección que usa el .NET 4.8 para empujar aquí.
   */
  sesion?: string;
}

/** Sobre {sesion, payload} que se intercambia con el backend .NET 4.8. */
export interface Envelope {
  sesion: string;
  payload: string;
}

export type ConnectionStatus =
  | 'disconnected'
  | 'connecting'
  | 'connected'
  | 'rejected'
  | 'error'
  | 'closed-by-server';

/** Códigos de cierre del POC (SECURITY-CHECKLIST.md §9). */
export const CLOSE_REASONS: Record<number, string> = {
  1000: 'Cierre normal',
  1006: 'Cierre anormal — el servidor rechazó el handshake o cortó la conexión',
  1001: 'GOING_AWAY — el servidor se está apagando',
  1009: 'Frame demasiado grande (límite del protocolo)',
  1012: 'SERVICE_RESTART — el servidor se reinicia; conviene reconectar',
  4001: 'TOKEN_MISSING — falta el token',
  4002: 'TOKEN_INVALID — firma o formato inválidos',
  4003: 'TOKEN_EXPIRED — token expirado',
  4004: 'TOKEN_CLAIMS_INVALID — issuer o audience incorrectos',
  4008: 'RATE_LIMIT_EXCEEDED — se superó el límite de mensajes por segundo',
  4009: 'MESSAGE_TOO_LARGE — mensaje mayor a 64 KB',
  4010: 'INVALID_PAYLOAD — el mensaje no cumple el esquema',
  4013: 'SERVER_AT_CAPACITY — se alcanzó el máximo de conexiones',
  4014: 'IDLE_TIMEOUT — conexión inactiva',
  4403: 'ORIGIN_NOT_ALLOWED — origen no permitido',
};

/**
 * Un frame entrante tal y como llegó por el cable, junto con su forma parseada.
 * Se conserva el `raw` porque el log muestra el JSON real, no una reconstrucción.
 */
export interface IncomingFrame {
  raw: string;
  msg: ServerMessage;
}

/** Resultado de un envío: el id para correlacionar y el frame exacto que salió. */
export interface SentFrame {
  id: string;
  raw: string;
}

export interface LogEntry {
  at: number;
  direction: 'out' | 'in' | 'system';
  /** Texto plano. Se renderiza SIEMPRE como texto, nunca como HTML. */
  text: string;
  level?: 'info' | 'warn' | 'error' | 'success';
  /**
   * El frame JSON que cruzó el cable, formateado para leerlo. Se renderiza como
   * texto igual que `text` — nunca como HTML — y va truncado (ver `MAX_FRAME_CHARS`).
   */
  json?: string;
}

/**
 * Tope de caracteres del frame mostrado en el log. La sonda de tamaño envía ~80 KB:
 * volcarlo entero congela el render y falsea el throughput que se está midiendo.
 */
export const MAX_FRAME_CHARS = 2000;

/** Métricas en vivo de una conexión. */
export interface LiveMetrics {
  handshakeMs: number | null;
  lastRttMs: number | null;
  avgRttMs: number | null;
  minRttMs: number | null;
  maxRttMs: number | null;
  p95RttMs: number | null;
  samples: number;
  /** Throughput instantáneo: mensajes recibidos en el último segundo. */
  msgsPerSec: number;
  sent: number;
  received: number;
}

export const EMPTY_METRICS: LiveMetrics = {
  handshakeMs: null,
  lastRttMs: null,
  avgRttMs: null,
  minRttMs: null,
  maxRttMs: null,
  p95RttMs: null,
  samples: 0,
  msgsPerSec: 0,
  sent: 0,
  received: 0,
};

export interface WsConfig {
  url: string;
  token: string;
}

/** Veredicto del endpoint de diagnóstico (GET /ws sin cabeceras de upgrade). */
export interface Diagnosis {
  allowed: boolean;
  reason?: string;
  code?: number;
}

export interface LoadTestResult {
  connections: number;
  msgsPerConnection: number;
  accepted: number;
  rejected: number;
  rejectedByReason: Record<string, number>;
  closedByServerMidTest: number;
  messagesSent: number;
  messagesEchoed: number;
  lostMessages: number;
  handshakeMs: { p50: number | null; p95: number | null; max: number | null };
  rttMs: { p50: number | null; p95: number | null; max: number | null };
  totalSeconds: number;
  avgMsgsPerSec: number;
  serverHealthyAfter: boolean;
  healthDetail: string;
  /**
   * Conexiones atendidas por cada réplica. Detrás de un balanceador revela el
   * reparto real; con un solo proceso siempre trae una única clave.
   */
  connectionsByInstance: Record<string, number>;
}
