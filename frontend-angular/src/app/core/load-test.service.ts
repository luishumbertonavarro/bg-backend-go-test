import { Injectable, signal } from '@angular/core';
import { Diagnosis, LoadTestResult, WsConfig } from './ws.models';

export interface LoadTestOptions {
  connections: number;
  msgsPerConnection: number;
  /** Mensajes por segundo y conexión. Mantener por debajo del rate limit del servidor. */
  ratePerSec: number;
}

export const DEFAULT_LOAD_OPTIONS: LoadTestOptions = {
  connections: 100,
  msgsPerConnection: 10,
  ratePerSec: 10,
};

/**
 * Prueba de carga desde el navegador.
 *
 * Aviso metodológico: Chrome/Edge limitan las conexiones WebSocket simultáneas por
 * origen y el propio JS compite por la CPU de la pestaña, así que estos números
 * sirven para ver el comportamiento en vivo, no como medida definitiva. Los datos
 * de referencia deben tomarse de `tools/loadtest.js`, que corre fuera del navegador.
 */
@Injectable()
export class LoadTestService {
  readonly running = signal(false);
  readonly progress = signal<{ opened: number; closed: number; total: number }>({
    opened: 0,
    closed: 0,
    total: 0,
  });
  readonly result = signal<LoadTestResult | null>(null);

  private abort = false;

  cancel(): void {
    this.abort = true;
  }

  async run(config: WsConfig, opts: LoadTestOptions): Promise<LoadTestResult> {
    this.abort = false;
    this.running.set(true);
    this.result.set(null);
    this.progress.set({ opened: 0, closed: 0, total: opts.connections });

    const stats = {
      accepted: 0,
      sent: 0,
      received: 0,
      handshakes: [] as number[],
      rtts: [] as number[],
      rejectedReasons: [] as string[],
      closedByServer: 0,
      byInstance: {} as Record<string, number>,
    };

    const t0 = performance.now();
    try {
      await Promise.all(
        Array.from({ length: opts.connections }, (_, i) => this.oneConnection(i, config, opts, stats)),
      );

      // Deja respirar al servidor y comprueba si sigue atendiendo con normalidad.
      await this.sleep(2000);
      const health = await this.healthCheck(config);
      const totalMs = performance.now() - t0;

      const byReason: Record<string, number> = {};
      for (const r of stats.rejectedReasons) byReason[r] = (byReason[r] ?? 0) + 1;

      const result: LoadTestResult = {
        connections: opts.connections,
        msgsPerConnection: opts.msgsPerConnection,
        accepted: stats.accepted,
        rejected: stats.rejectedReasons.length,
        rejectedByReason: byReason,
        closedByServerMidTest: stats.closedByServer,
        messagesSent: stats.sent,
        messagesEchoed: stats.received,
        lostMessages: stats.sent - stats.received,
        handshakeMs: {
          p50: this.pct(stats.handshakes, 50),
          p95: this.pct(stats.handshakes, 95),
          max: this.pct(stats.handshakes, 100),
        },
        rttMs: {
          p50: this.pct(stats.rtts, 50),
          p95: this.pct(stats.rtts, 95),
          max: this.pct(stats.rtts, 100),
        },
        totalSeconds: +(totalMs / 1000).toFixed(2),
        avgMsgsPerSec: +(stats.received / (totalMs / 1000) || 0).toFixed(1),
        serverHealthyAfter: health.ok,
        healthDetail: health.detail,
        connectionsByInstance: stats.byInstance,
      };

      this.result.set(result);
      return result;
    } finally {
      this.running.set(false);
    }
  }

  // --------------------------------------------------------------- internos

  private oneConnection(
    i: number,
    config: WsConfig,
    opts: LoadTestOptions,
    stats: {
      accepted: number; sent: number; received: number;
      handshakes: number[]; rtts: number[]; rejectedReasons: string[]; closedByServer: number;
      byInstance: Record<string, number>;
    },
  ): Promise<void> {
    return new Promise<void>((resolve) => {
      if (this.abort) return resolve();

      const url = this.buildUrl(config);
      const t0 = performance.now();
      let socket: WebSocket;
      try {
        socket = new WebSocket(url);
      } catch {
        stats.rejectedReasons.push('CLIENT_ERROR');
        return resolve();
      }

      const inflight = new Map<string, number>();
      let instanceSeen: string | null = null;
      let opened = false;
      let sent = 0;
      let timer: ReturnType<typeof setInterval> | null = null;
      let settled = false;

      const settle = async (closeCode?: number) => {
        if (settled) return;
        settled = true;
        if (timer) clearInterval(timer);
        if (!opened) {
          const d = await this.diagnose(config);
          stats.rejectedReasons.push(d.allowed ? 'UNREACHABLE' : (d.reason ?? 'UNKNOWN'));
        } else if (closeCode !== undefined && closeCode >= 4000) {
          stats.closedByServer++;
        }
        this.progress.update((p) => ({ ...p, closed: p.closed + 1 }));
        resolve();
      };

      socket.onopen = () => {
        opened = true;
        stats.accepted++;
        stats.handshakes.push(performance.now() - t0);
        this.progress.update((p) => ({ ...p, opened: p.opened + 1 }));

        timer = setInterval(() => {
          if (this.abort || sent >= opts.msgsPerConnection) {
            if (timer) clearInterval(timer);
            // Margen para que lleguen los ecos pendientes antes de cerrar.
            setTimeout(() => {
              if (socket.readyState === WebSocket.OPEN) socket.close(1000, 'done');
              else void settle();
            }, 1500);
            return;
          }
          const id = `lt${i}-${sent}`;
          inflight.set(id, performance.now());
          try {
            socket.send(JSON.stringify({ type: 'echo', id, ts: Date.now(), payload: `carga-${i}-${sent}` }));
            sent++;
            stats.sent++;
          } catch {
            if (timer) clearInterval(timer);
          }
        }, Math.max(1, Math.floor(1000 / opts.ratePerSec)));
      };

      socket.onmessage = (ev) => {
        let msg: { id?: string; instance?: string };
        try { msg = JSON.parse(String(ev.data)); } catch { return; }
        // Se anota una sola vez por conexión: interesa a qué réplica fue el
        // socket, no cuántos mensajes devolvió.
        if (msg.instance && !instanceSeen) {
          instanceSeen = msg.instance;
          stats.byInstance[msg.instance] = (stats.byInstance[msg.instance] ?? 0) + 1;
        }
        const started = msg.id ? inflight.get(msg.id) : undefined;
        if (started !== undefined) {
          inflight.delete(msg.id!);
          stats.rtts.push(performance.now() - started);
          stats.received++;
        }
      };

      socket.onerror = () => { /* el motivo se resuelve en onclose vía diagnose() */ };
      socket.onclose = (ev) => void settle(ev.code);
    });
  }

  private healthCheck(config: WsConfig): Promise<{ ok: boolean; detail: string }> {
    return new Promise((resolve) => {
      let socket: WebSocket;
      try {
        socket = new WebSocket(this.buildUrl(config));
      } catch (e) {
        return resolve({ ok: false, detail: String(e) });
      }
      const t0 = performance.now();
      const done = (r: { ok: boolean; detail: string }) => {
        clearTimeout(to);
        try { socket.close(); } catch { /* ya cerrado */ }
        resolve(r);
      };
      const to = setTimeout(() => done({ ok: false, detail: 'sin respuesta en 5 s' }), 5000);

      socket.onopen = () =>
        socket.send(JSON.stringify({ type: 'ping', id: 'health', ts: Date.now(), payload: '' }));
      socket.onmessage = (ev) => {
        let m: { type?: string };
        try { m = JSON.parse(String(ev.data)); } catch { return done({ ok: false, detail: 'respuesta ilegible' }); }
        done({
          ok: m.type === 'pong',
          detail: m.type === 'pong'
            ? `pong en ${(performance.now() - t0).toFixed(0)} ms`
            : `respuesta inesperada: ${m.type}`,
        });
      };
      socket.onerror = () => done({ ok: false, detail: 'error de conexión tras la carga' });
    });
  }

  private async diagnose(config: WsConfig): Promise<Diagnosis> {
    try {
      const res = await fetch(this.buildUrl(config).replace(/^ws/, 'http'));
      return (await res.json()) as Diagnosis;
    } catch {
      return { allowed: false, reason: 'SERVIDOR_INALCANZABLE' };
    }
  }

  private buildUrl(config: WsConfig): string {
    const u = new URL(config.url);
    if (config.token) u.searchParams.set('token', config.token);
    return u.toString();
  }

  private pct(arr: number[], p: number): number | null {
    if (!arr.length) return null;
    const s = [...arr].sort((a, b) => a - b);
    return +s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))].toFixed(2);
  }

  private sleep(ms: number): Promise<void> {
    return new Promise((r) => setTimeout(r, ms));
  }
}
