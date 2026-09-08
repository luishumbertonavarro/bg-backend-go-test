import { Injectable, OnDestroy, signal } from '@angular/core';
import { BehaviorSubject, Observable, Subject } from 'rxjs';
import {
  ClientMessage,
  ConnectionStatus,
  Diagnosis,
  EMPTY_METRICS,
  IncomingFrame,
  LiveMetrics,
  SentFrame,
  ServerMessage,
  WsConfig,
} from './ws.models';

/**
 * Cliente WebSocket parametrizable por URL, con instrumentación de métricas.
 *
 * NO es `providedIn: 'root'`: cada panel de backend provee su propia instancia
 * (`providers: [WebSocketService]`), de modo que cada pestaña mantiene su
 * conexión y sus métricas independientes.
 */
@Injectable()
export class WebSocketService implements OnDestroy {
  private socket: WebSocket | null = null;
  private config: WsConfig | null = null;

  /** id de mensaje -> instante de envío (performance.now()), para calcular el RTT. */
  private readonly inflight = new Map<string, number>();
  private rtts: number[] = [];
  private seq = 0;

  private throughputTimer: ReturnType<typeof setInterval> | null = null;
  private receivedInWindow = 0;

  private readonly status$$ = new BehaviorSubject<ConnectionStatus>('disconnected');
  private readonly messages$$ = new Subject<IncomingFrame>();
  private readonly metrics$$ = new BehaviorSubject<LiveMetrics>({ ...EMPTY_METRICS });
  private readonly closes$$ = new Subject<{ code: number; reason: string }>();

  /** Espejo en signals para consumo directo desde plantillas. */
  readonly status = signal<ConnectionStatus>('disconnected');
  readonly metrics = signal<LiveMetrics>({ ...EMPTY_METRICS });
  readonly lastCloseCode = signal<number | null>(null);
  readonly lastRejectReason = signal<string | null>(null);
  /** Réplica que respondió el último frame; null si el backend no lo informa. */
  readonly instance = signal<string | null>(null);
  /**
   * Sesión de esta conexión, según el claim `sid` del token. Es la dirección a
   * la que el backend .NET 4.8 empuja mensajes con POST /api/push.
   */
  readonly sesion = signal<string | null>(null);

  get status$(): Observable<ConnectionStatus> { return this.status$$.asObservable(); }
  get messages$(): Observable<IncomingFrame> { return this.messages$$.asObservable(); }
  get metrics$(): Observable<LiveMetrics> { return this.metrics$$.asObservable(); }
  get closes$(): Observable<{ code: number; reason: string }> { return this.closes$$.asObservable(); }

  get isOpen(): boolean { return this.socket?.readyState === WebSocket.OPEN; }

  // ---------------------------------------------------------------- conexión

  connect(config: WsConfig): void {
    this.disconnect();
    this.config = config;
    this.resetMetrics();
    this.lastCloseCode.set(null);
    this.lastRejectReason.set(null);
    this.instance.set(null);
    this.sesion.set(null);
    this.setStatus('connecting');

    const url = this.buildUrl(config);
    const startedAt = performance.now();
    let opened = false;

    let socket: WebSocket;
    try {
      socket = new WebSocket(url);
    } catch (e) {
      this.setStatus('error');
      this.lastRejectReason.set(`URL inválida: ${String(e)}`);
      return;
    }
    this.socket = socket;

    socket.onopen = () => {
      opened = true;
      this.patchMetrics({ handshakeMs: +(performance.now() - startedAt).toFixed(2) });
      this.setStatus('connected');
      this.startThroughputWindow();
    };

    socket.onmessage = (ev) => this.onFrame(ev.data);

    socket.onerror = () => {
      // El navegador no expone el motivo; se resuelve en onclose vía diagnose().
      if (!opened) this.setStatus('connecting');
    };

    socket.onclose = (ev) => {
      this.stopThroughputWindow();
      this.lastCloseCode.set(ev.code);
      this.closes$$.next({ code: ev.code, reason: ev.reason });

      if (!opened) {
        // Rechazo en el handshake: el navegador solo entrega 1006, así que
        // preguntamos al endpoint de diagnóstico por el motivo real.
        this.setStatus('rejected');
        void this.diagnose(config).then((d) => {
          this.lastRejectReason.set(d.allowed ? 'Servidor inalcanzable' : (d.reason ?? 'desconocido'));
          if (d.code) this.lastCloseCode.set(d.code);
        });
      } else if (ev.code >= 4000) {
        this.setStatus('closed-by-server');
        this.lastRejectReason.set(ev.reason || null);
      } else {
        this.setStatus('disconnected');
      }
      this.socket = null;
    };
  }

  disconnect(): void {
    this.stopThroughputWindow();
    const s = this.socket;
    this.socket = null;
    if (s && (s.readyState === WebSocket.OPEN || s.readyState === WebSocket.CONNECTING)) {
      s.onclose = null;
      s.onerror = null;
      s.onmessage = null;
      try { s.close(1000, 'client disconnect'); } catch { /* ya cerrado */ }
    }
    this.inflight.clear();
    this.setStatus('disconnected');
  }

  // ---------------------------------------------------------------- mensajes

  /** Envía un eco. Devuelve el frame enviado, o null si el socket no está abierto. */
  send(payload: string): SentFrame | null {
    return this.dispatch('echo', payload);
  }

  /** Envía un ping (el servidor responde `pong`); sirve para medir RTT puro. */
  ping(): SentFrame | null {
    return this.dispatch('ping', '');
  }

  /** Envía texto crudo sin pasar por el esquema — para probar el rechazo del servidor. */
  sendRaw(raw: string): boolean {
    if (!this.isOpen) return false;
    this.socket!.send(raw);
    this.patchMetrics({ sent: this.metrics().sent + 1 });
    return true;
  }

  private dispatch(type: 'echo' | 'ping', payload: string): SentFrame | null {
    if (!this.isOpen) return null;
    const id = `m${++this.seq}-${Date.now().toString(36)}`;
    this.inflight.set(id, performance.now());
    // El frame se serializa una sola vez: lo que se envía es exactamente lo que se devuelve.
    const raw = JSON.stringify({ type, id, ts: Date.now(), payload } satisfies ClientMessage);
    this.socket!.send(raw);
    this.patchMetrics({ sent: this.metrics().sent + 1 });
    return { id, raw };
  }

  private onFrame(data: unknown): void {
    if (typeof data !== 'string') return;
    let msg: ServerMessage;
    try {
      msg = JSON.parse(data) as ServerMessage;
    } catch {
      this.messages$$.next({
        raw: data,
        msg: { type: 'error', id: '-', ts: Date.now(), reason: 'RESPUESTA_NO_JSON' },
      });
      return;
    }

    const startedAt = this.inflight.get(msg.id);
    if (startedAt !== undefined) {
      this.inflight.delete(msg.id);
      this.recordRtt(performance.now() - startedAt);
    }
    if (msg.instance) this.instance.set(msg.instance);
    if (msg.sesion) this.sesion.set(msg.sesion);
    this.receivedInWindow++;
    this.patchMetrics({ received: this.metrics().received + 1 });
    this.messages$$.next({ raw: data, msg });
  }

  // ---------------------------------------------------------------- métricas

  private recordRtt(rtt: number): void {
    this.rtts.push(rtt);
    // Ventana acotada para que una sesión larga no crezca sin límite.
    if (this.rtts.length > 2000) this.rtts = this.rtts.slice(-1000);

    const sorted = [...this.rtts].sort((a, b) => a - b);
    const r2 = (n: number) => +n.toFixed(2);
    this.patchMetrics({
      lastRttMs: r2(rtt),
      minRttMs: r2(sorted[0]),
      maxRttMs: r2(sorted[sorted.length - 1]),
      avgRttMs: r2(this.rtts.reduce((a, b) => a + b, 0) / this.rtts.length),
      p95RttMs: r2(sorted[Math.min(sorted.length - 1, Math.floor(0.95 * sorted.length))]),
      samples: this.rtts.length,
    });
  }

  private startThroughputWindow(): void {
    this.stopThroughputWindow();
    this.receivedInWindow = 0;
    this.throughputTimer = setInterval(() => {
      this.patchMetrics({ msgsPerSec: this.receivedInWindow });
      this.receivedInWindow = 0;
    }, 1000);
  }

  private stopThroughputWindow(): void {
    if (this.throughputTimer) {
      clearInterval(this.throughputTimer);
      this.throughputTimer = null;
    }
  }

  private resetMetrics(): void {
    this.rtts = [];
    this.inflight.clear();
    this.receivedInWindow = 0;
    const fresh = { ...EMPTY_METRICS };
    this.metrics.set(fresh);
    this.metrics$$.next(fresh);
  }

  private patchMetrics(patch: Partial<LiveMetrics>): void {
    const next = { ...this.metrics(), ...patch };
    this.metrics.set(next);
    this.metrics$$.next(next);
  }

  private setStatus(s: ConnectionStatus): void {
    this.status.set(s);
    this.status$$.next(s);
  }

  // ------------------------------------------------------------ diagnóstico

  /**
   * Consulta `GET /ws` (sin cabeceras de upgrade) para conocer el motivo exacto de un
   * rechazo, que la API WebSocket del navegador oculta tras un genérico 1006.
   */
  async diagnose(config: WsConfig): Promise<Diagnosis> {
    try {
      const http = this.buildUrl(config).replace(/^ws/, 'http');
      const res = await fetch(http, { method: 'GET' });
      const body = (await res.json()) as Diagnosis;
      return body;
    } catch {
      return { allowed: false, reason: 'SERVIDOR_INALCANZABLE' };
    }
  }

  private buildUrl(config: WsConfig): string {
    const u = new URL(config.url);
    if (config.token) u.searchParams.set('token', config.token);
    return u.toString();
  }

  ngOnDestroy(): void {
    this.disconnect();
    this.messages$$.complete();
    this.metrics$$.complete();
    this.status$$.complete();
    this.closes$$.complete();
  }
}
