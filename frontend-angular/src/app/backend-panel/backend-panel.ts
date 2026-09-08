import { Component, DestroyRef, inject, input, OnInit, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { DecimalPipe, JsonPipe } from '@angular/common';
import { FormsModule } from '@angular/forms';
import { WebSocketService } from '../core/websocket.service';
import { DEFAULT_LOAD_OPTIONS, LoadTestOptions, LoadTestService } from '../core/load-test.service';
import { BackendDescriptor, CLOSE_REASONS, Envelope, LogEntry, MAX_FRAME_CHARS } from '../core/ws.models';

@Component({
  selector: 'app-backend-panel',
  standalone: true,
  imports: [FormsModule, DecimalPipe, JsonPipe],
  templateUrl: './backend-panel.html',
  styleUrl: './backend-panel.css',
  // Una instancia de cada servicio POR PESTAÑA: conexiones y métricas independientes.
  providers: [WebSocketService, LoadTestService],
})
export class BackendPanel implements OnInit {
  readonly backend = input.required<BackendDescriptor>();

  readonly ws = inject(WebSocketService);
  readonly load = inject(LoadTestService);
  private readonly destroyRef = inject(DestroyRef);

  readonly url = signal('');
  readonly token = signal('');
  readonly outgoing = signal('hola desde Angular 20');
  readonly log = signal<LogEntry[]>([]);
  readonly loadOpts = signal<LoadTestOptions>({ ...DEFAULT_LOAD_OPTIONS });
  readonly autoPing = signal(false);

  /** Sobres {sesion, payload} que se le enviarían al backend .NET 4.8. */
  readonly outbox = signal<Envelope[]>([]);
  readonly outboxUrl = signal<string | null>(null);
  readonly outboxError = signal<string | null>(null);
  readonly pushText = signal('aviso desde el .NET 4.8');

  private autoPingTimer: ReturnType<typeof setInterval> | null = null;

  ngOnInit(): void {
    this.url.set(this.backend().defaultUrl);

    this.ws.messages$.pipe(takeUntilDestroyed(this.destroyRef)).subscribe(({ msg, raw }) => {
      const rtt = this.ws.metrics().lastRttMs;
      if (msg.type === 'error') {
        this.append('in', `error del servidor: ${msg.reason ?? 'sin motivo'}`, 'error', raw);
      } else if (msg.type === 'push') {
        // No es respuesta a nada nuestro: lo empujó el .NET 4.8 a esta sesión.
        this.append('in', `push del .NET 4.8: ${msg.payload ?? ''}`, 'warn', raw);
      } else {
        this.append(
          'in',
          `${msg.type} id=${msg.id}${rtt !== null ? ` · RTT ${rtt} ms` : ''}` +
            `${msg.instance ? ` · instancia ${msg.instance}` : ''}`,
          'success',
          raw,
        );
      }
    });

    this.ws.closes$.pipe(takeUntilDestroyed(this.destroyRef)).subscribe(({ code, reason }) => {
      const known = CLOSE_REASONS[code];
      this.append(
        'system',
        `conexión cerrada · código ${code}${known ? ` · ${known}` : ''}${reason ? ` · ${reason}` : ''}`,
        code === 1000 ? 'info' : 'warn',
      );
      this.stopAutoPing();
    });

    this.destroyRef.onDestroy(() => this.stopAutoPing());
  }

  // ------------------------------------------------------------- conexión

  connect(): void {
    this.append('system', `conectando a ${this.url()}…`);
    this.ws.connect({ url: this.url(), token: this.token().trim() });
  }

  disconnect(): void {
    this.stopAutoPing();
    this.ws.disconnect();
    this.append('system', 'desconectado por el cliente');
  }

  // ------------------------------------------------------------- mensajes

  send(): void {
    const sent = this.ws.send(this.outgoing());
    if (sent) this.append('out', `echo id=${sent.id}`, 'info', sent.raw);
    else this.append('system', 'no se pudo enviar: el socket no está abierto', 'warn');
  }

  ping(): void {
    const sent = this.ws.ping();
    if (sent) this.append('out', `ping id=${sent.id}`, 'info', sent.raw);
  }

  toggleAutoPing(): void {
    if (this.autoPing()) { this.stopAutoPing(); return; }
    this.autoPing.set(true);
    // 10 ping/s: por debajo del límite de 20 msg/s del servidor.
    this.autoPingTimer = setInterval(() => {
      if (!this.ws.isOpen) { this.stopAutoPing(); return; }
      this.ws.ping();
    }, 100);
    this.append('system', 'ping automático a 10 msg/s (bajo el límite de 20)');
  }

  private stopAutoPing(): void {
    if (this.autoPingTimer) clearInterval(this.autoPingTimer);
    this.autoPingTimer = null;
    this.autoPing.set(false);
  }

  // -------------------------------------------- pruebas de control de seguridad

  /** Supera el rate limit a propósito: el servidor debe cerrar con 4008. */
  probeRateLimit(): void {
    if (!this.ws.isOpen) return this.append('system', 'conéctate primero', 'warn');
    this.append('out', '60 mensajes de golpe — se espera cierre 4008 RATE_LIMIT_EXCEEDED', 'warn');
    for (let i = 0; i < 60; i++) this.ws.send(`burst-${i}`);
  }

  /** Envía más de 64 KB: el servidor debe cerrar con 4009. */
  probeOversize(): void {
    if (!this.ws.isOpen) return this.append('system', 'conéctate primero', 'warn');
    const raw = JSON.stringify({ type: 'echo', id: 'oversize', ts: Date.now(), payload: 'A'.repeat(80_000) });
    this.append('out', 'mensaje de ~80 KB — se espera cierre 4009 MESSAGE_TOO_LARGE', 'warn', raw);
    this.ws.sendRaw(raw);
  }

  /** Payload con script: debe rechazarse por sanitización (4010). */
  probeXss(): void {
    if (!this.ws.isOpen) return this.append('system', 'conéctate primero', 'warn');
    this.append('out', 'payload con <script> — se espera error/cierre 4010 INVALID_PAYLOAD', 'warn');
    this.ws.send('<script>alert(1)</script>');
  }

  /** JSON malformado: debe rechazarse con 4010. */
  probeMalformed(): void {
    if (!this.ws.isOpen) return this.append('system', 'conéctate primero', 'warn');
    const raw = '{"type":"echo","id":';
    this.append('out', 'JSON roto — se espera error/cierre 4010 INVALID_PAYLOAD', 'warn', raw);
    this.ws.sendRaw(raw);
  }

  // ------------------------------------------------------------ carga

  async runLoadTest(): Promise<void> {
    const opts = this.loadOpts();
    this.append('system', `prueba de carga: ${opts.connections} conexiones x ${opts.msgsPerConnection} mensajes`);
    const r = await this.load.run({ url: this.url(), token: this.token().trim() }, opts);
    this.append(
      'system',
      `carga terminada · aceptadas ${r.accepted}/${r.connections} · rechazadas ${r.rejected} · ` +
        `${r.avgMsgsPerSec} msg/s · servidor sano después: ${r.serverHealthyAfter ? 'sí' : 'NO'}`,
      r.serverHealthyAfter ? 'success' : 'error',
    );
  }

  /** Las plantillas de Angular no admiten spread, así que el patch vive aquí. */
  setLoadOption(key: keyof LoadTestOptions, value: number): void {
    if (!Number.isFinite(value) || value < 1) return;
    this.loadOpts.update((o) => ({ ...o, [key]: value }));
  }

  // ------------------------------------------------------------ utilidades

  clearLog(): void {
    this.log.set([]);
  }

  statusLabel(): string {
    switch (this.ws.status()) {
      case 'connected': return 'Conectado';
      case 'connecting': return 'Conectando…';
      case 'rejected': return 'Rechazado';
      case 'error': return 'Error';
      case 'closed-by-server': return 'Cerrado por el servidor';
      default: return 'Desconectado';
    }
  }

  rejectDetail(): string | null {
    const reason = this.ws.lastRejectReason();
    const code = this.ws.lastCloseCode();
    if (!reason && code === null) return null;
    const known = code !== null ? CLOSE_REASONS[code] : undefined;
    return [reason, known].filter(Boolean).join(' · ') || null;
  }

  rejectedByReasonEntries(): Array<[string, number]> {
    const r = this.load.result();
    return r ? Object.entries(r.rejectedByReason) : [];
  }

  /** Reparto de las conexiones de la prueba de carga entre réplicas, de mayor a menor. */
  connectionsByInstanceEntries(): Array<[string, number]> {
    const r = this.load.result();
    return r ? Object.entries(r.connectionsByInstance).sort((a, b) => b[1] - a[1]) : [];
  }

  time(at: number): string {
    const d = new Date(at);
    return `${d.toLocaleTimeString('es', { hour12: false })}.${String(d.getMilliseconds()).padStart(3, '0')}`;
  }

  // ---------------------------------------------------- puente con el .NET 4.8

  /** Base HTTP del backend, derivada de la URL del WebSocket. */
  private apiBase(): string {
    return this.url().replace(/^ws/, 'http').replace(/\/ws$/, '');
  }

  /**
   * Consulta lo que el backend le enviaría al .NET 4.8. Mientras no haya URL del
   * .NET configurada, los sobres se quedan aquí para poder inspeccionarlos.
   */
  async refreshOutbox(): Promise<void> {
    this.outboxError.set(null);
    try {
      const res = await fetch(`${this.apiBase()}/api/outbox`, {
        headers: { Authorization: `Bearer ${this.token()}` },
      });
      if (!res.ok) {
        this.outboxError.set(`HTTP ${res.status}`);
        return;
      }
      const body = await res.json();
      this.outbox.set(body.items ?? []);
      this.outboxUrl.set(body.webhookUrl || null);
    } catch {
      // Mismo motivo que en el diagnóstico: sin CORS el navegador ni deja verlo.
      this.outboxError.set('SERVIDOR_INALCANZABLE');
    }
  }

  /** Simula al .NET 4.8 empujando a esta misma sesión. */
  async sendPush(): Promise<void> {
    const sesion = this.ws.sesion();
    if (!sesion) return;
    this.append('system', `simulando push del .NET 4.8 a la sesión ${sesion}`, 'info');
    try {
      const res = await fetch(`${this.apiBase()}/api/push`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${this.token()}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ sesion, payload: this.pushText() }),
      });
      const body = await res.json().catch(() => ({}));
      this.append(
        'system',
        `POST /api/push -> HTTP ${res.status} ${body.reason ?? `entregado a ${body.delivered} conexión(es)`}`,
        res.ok ? 'success' : 'error',
      );
    } catch {
      this.append('system', 'POST /api/push -> SERVIDOR_INALCANZABLE', 'error');
    }
  }

  private append(
    direction: LogEntry['direction'],
    text: string,
    level: LogEntry['level'] = 'info',
    raw?: string,
  ): void {
    const entry: LogEntry = { at: Date.now(), direction, text, level };
    if (raw !== undefined) entry.json = this.formatFrame(raw);
    // Ventana acotada: un log infinito degrada el render y falsea el throughput medido.
    this.log.update((entries) => [entry, ...entries].slice(0, 400));
  }

  /**
   * Deja el frame legible sin mentir sobre lo que viajó: si es JSON válido se indenta,
   * y si no lo es (sonda de JSON malformado) se muestra tal cual, sin tocarlo.
   */
  private formatFrame(raw: string): string {
    let out = raw;
    try {
      out = JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      /* no es JSON: se muestra el crudo, que es justo lo que la sonda quiere enseñar */
    }
    return out.length > MAX_FRAME_CHARS
      ? `${out.slice(0, MAX_FRAME_CHARS)}\n… (truncado: ${out.length} caracteres en total)`
      : out;
  }
}
