import { existsSync, readFileSync } from 'node:fs';
import { join, extname, resolve } from 'node:path';
import {
  mockAuditLogs,
  mockConnectionConfigured,
  mockDeliveries,
  mockMonitorSettings,
  mockNotificationChannels,
  mockNotificationProviders,
  mockPollRuns,
  mockQrSettings,
  mockStatusHealthy,
  mockTransactions,
} from './mock-data';

const port = Number(process.env.PORT || process.env.LISTEN_ADDR?.split(':')?.[1] || 18081);
const candidates = [
  resolve(import.meta.dir, '../../dist'),
  resolve(import.meta.dir, '../../../internal/httpui/dist'),
];

let distDir = candidates.find((c) => existsSync(c)) || candidates[1];

const mimeTypes: Record<string, string> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'application/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.ico': 'image/x-icon',
};

// Mutable runtime state for tests
let runtimeState = {
  status: { ...mockStatusHealthy },
  connection: { ...mockConnectionConfigured },
  channels: JSON.parse(JSON.stringify(mockNotificationChannels.items)),
  transactions: JSON.parse(JSON.stringify(mockTransactions.items)),
  monitorSettings: { ...mockMonitorSettings },
  qrSettings: { ...mockQrSettings },
  paymentQr: {
    configured: false,
    hasImage: false,
    qr: null as any,
  },
  activeAuthAttempt: null as any,
};

function resetState() {
  runtimeState = {
    status: { ...mockStatusHealthy },
    connection: { ...mockConnectionConfigured },
    channels: JSON.parse(JSON.stringify(mockNotificationChannels.items)),
    transactions: JSON.parse(JSON.stringify(mockTransactions.items)),
    monitorSettings: { ...mockMonitorSettings },
    qrSettings: { ...mockQrSettings },
    paymentQr: {
      configured: false,
      hasImage: false,
      qr: null,
    },
    activeAuthAttempt: null,
  };
}

// SSE Subscribers
const sseClients = new Set<(msg: string) => void>();

export function broadcastSse(eventType: string, data: any, id?: string) {
  let payload = '';
  if (id) {
    payload += `id: ${id}\n`;
  }
  payload += `event: ${eventType}\ndata: ${JSON.stringify(data)}\n\n`;
  for (const send of sseClients) {
    try {
      send(payload);
    } catch {}
  }
}

const server = Bun.serve({
  port,
  async fetch(req) {
    const url = new URL(req.url);
    const path = url.pathname;
    const method = req.method;

    // Health check
    if (path === '/healthz') {
      return new Response(JSON.stringify({ status: 'HEALTHY' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // CSRF
    if (path === '/api/v1/csrf') {
      return new Response(JSON.stringify({ token: 'fixture-csrf-token' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Status
    if (path === '/api/v1/status') {
      return new Response(JSON.stringify(runtimeState.status), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Connection
    if (path === '/api/v1/connection/configure' && method === 'POST') {
      const body = await req.json().catch(() => ({}));
      runtimeState.connection = {
        configured: true,
        connection: {
          id: 'conn_1',
          state: 'MONITORING',
          accountMasked: body.accountMasked || '***1234',
          generation: 1,
          updatedAt: new Date().toISOString(),
        },
      };
      return new Response(JSON.stringify(runtimeState.connection), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path === '/api/v1/connection') {
      if (method === 'POST') {
        const body = await req.json().catch(() => ({}));
        runtimeState.connection = {
          configured: true,
          connection: {
            id: 'conn_1',
            state: 'MONITORING',
            accountMasked: body.accountMasked || '***1234',
            generation: 1,
            updatedAt: new Date().toISOString(),
          },
        };
        return new Response(JSON.stringify(runtimeState.connection), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify(runtimeState.connection), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Auth start/status/cancel
    if (path === '/api/v1/connection/auth/start') {
      runtimeState.activeAuthAttempt = {
        attemptId: `auth_${Date.now()}`,
        status: 'AWAITING_USER_LOGIN',
        screenUrl: '/mock-vnc.html',
        expiresAt: new Date(Date.now() + 900000).toISOString(),
      };
      return new Response(JSON.stringify(runtimeState.activeAuthAttempt), {
        status: 201,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path.startsWith('/api/v1/connection/auth/') && path.endsWith('/status')) {
      return new Response(
        JSON.stringify({ status: runtimeState.activeAuthAttempt?.status || 'AWAITING_USER_LOGIN' }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    }
    if (path.startsWith('/api/v1/connection/auth/') && path.endsWith('/cancel')) {
      runtimeState.activeAuthAttempt = null;
      return new Response(JSON.stringify({ cancelled: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Channels
    if (path === '/api/v1/notification-channels/providers') {
      return new Response(JSON.stringify(mockNotificationProviders), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path === '/api/v1/notification-channels') {
      if (method === 'POST') {
        const body = await req.json().catch(() => ({}));
        const newChan = {
          id: `chan_${Date.now()}`,
          name: body.name || 'New Channel',
          provider: body.provider || 'WEBHOOK',
          status: 'DISABLED',
          destination: body.destination || body.url || 'https://example.com',
          secret: body.provider === 'WEBHOOK' ? 'whsec_sample_secret_key_12345' : undefined,
          barkConfig: body.barkConfig,
          createdAt: new Date().toISOString(),
        };
        runtimeState.channels.push(newChan);
        return new Response(JSON.stringify(newChan), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify({ items: runtimeState.channels }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path.startsWith('/api/v1/notification-channels/') && path.endsWith('/test')) {
      return new Response(
        JSON.stringify({ success: true, message: 'Bark ???? ch???p nh???n th??ng b??o th???' }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    }
    if (path.startsWith('/api/v1/notification-channels/') && path.endsWith('/rotate-secret')) {
      const parts = path.split('/');
      const id = parts[4];
      const body = await req.json().catch(() => ({}));
      const chan = runtimeState.channels.find((c: any) => c.id === id);
      if (chan && body.deviceKey) {
        chan.destination = body.deviceKey;
      }
      return new Response(JSON.stringify({ status: 'ACTIVE' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path.startsWith('/api/v1/notification-channels/') && (path.endsWith('/enable') || path.endsWith('/disable') || path.endsWith('/toggle'))) {
      const parts = path.split('/');
      const id = parts[4];
      const isEnable = path.endsWith('/enable');
      const isDisable = path.endsWith('/disable');
      const chan = runtimeState.channels.find((c: any) => c.id === id);
      if (chan) {
        if (isEnable) chan.status = 'ACTIVE';
        else if (isDisable) chan.status = 'DISABLED';
        else chan.status = chan.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE';
      }
      return new Response(JSON.stringify({ status: chan?.status || 'ACTIVE' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path.startsWith('/api/v1/notification-channels/') && method === 'PUT') {
      const parts = path.split('/');
      const id = parts[4];
      const body = await req.json().catch(() => ({}));
      const chan = runtimeState.channels.find((c: any) => c.id === id);
      if (chan) {
        Object.assign(chan, body);
      }
      return new Response(JSON.stringify(chan || { success: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path.startsWith('/api/v1/notification-channels/') && method === 'DELETE') {
      const parts = path.split('/');
      const id = parts[4];
      runtimeState.channels = runtimeState.channels.filter((c: any) => c.id !== id);
      return new Response(JSON.stringify({ success: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Transactions
    if (path === '/api/v1/transactions') {
      const type = url.searchParams.get('type');
      let filtered = [...runtimeState.transactions];
      if (type === 'credit') {
        filtered = filtered.filter((tx: any) => tx.credit > 0);
      } else if (type === 'debit') {
        filtered = filtered.filter((tx: any) => tx.debit > 0);
      }
      const incoming = filtered.reduce((sum: number, tx: any) => sum + (tx.credit || 0), 0);
      const outgoing = filtered.reduce((sum: number, tx: any) => sum + (tx.debit || 0), 0);
      return new Response(
        JSON.stringify({
          items: filtered,
          summary: { count: filtered.length, incoming, outgoing },
          cursor: null,
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    }
    if (path === '/api/v1/transactions/ensure-history') {
      return new Response(JSON.stringify({ status: 'OK', count: 0 }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path.startsWith('/api/v1/transactions/')) {
      const id = path.replace('/api/v1/transactions/', '');
      const found = runtimeState.transactions.find((tx: any) => tx.id === id);
      if (!found) {
        return new Response(JSON.stringify({ error: 'Kh??ng t??m th???y giao d???ch' }), {
          status: 404,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify(found), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Activity
    if (path === '/api/v1/poll-runs') {
      return new Response(JSON.stringify(mockPollRuns), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path === '/api/v1/deliveries') {
      return new Response(JSON.stringify(mockDeliveries), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path.startsWith('/api/v1/deliveries/') && path.endsWith('/replay')) {
      return new Response(JSON.stringify({ success: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path === '/api/v1/audit-logs') {
      return new Response(JSON.stringify(mockAuditLogs), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Monitor & QR settings
    if (path === '/api/v1/monitor/settings') {
      if (method === 'POST') {
        const body = await req.json().catch(() => ({}));
        if (body.windows) {
          runtimeState.monitorSettings.settings = {
            ...runtimeState.monitorSettings.settings,
            ...body,
          };
        }
        return new Response(JSON.stringify(runtimeState.monitorSettings), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify(runtimeState.monitorSettings), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path === '/api/v1/payment-qr') {
      return new Response(
        JSON.stringify(runtimeState.paymentQr),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    }
    if (path === '/api/v1/payment-qr/generate') {
      const body = await req.json().catch(() => ({}));
      runtimeState.paymentQr = {
        configured: true,
        hasImage: true,
        qr: {
          accountNumber: body.accountNumber || '123456789',
          accountName: body.accountName || 'NGUYEN VIET TUAN',
          bankBin: '970416',
          bankName: 'ACB',
          imageUrl: 'https://api.vietqr.io/img/ACB.png',
        },
      };
      return new Response(
        JSON.stringify(runtimeState.paymentQr),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    }
    if (path === '/api/v1/qr/settings') {
      if (method === 'POST') {
        const body = await req.json().catch(() => ({}));
        runtimeState.qrSettings = { ...runtimeState.qrSettings, ...body };
        return new Response(JSON.stringify(runtimeState.qrSettings), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify(runtimeState.qrSettings), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Realtime SSE endpoint
    if (path === '/api/v1/events') {
      const lastEventId = req.headers.get('Last-Event-ID') || url.searchParams.get('last_event_id');
      const stream = new ReadableStream({
        start(controller) {
          const send = (msg: string) => {
            controller.enqueue(new TextEncoder().encode(msg));
          };
          sseClients.add(send);

          // Initial handshake
          send(`event: initial_state\ndata: {"watermark":100,"reconnectId":${JSON.stringify(lastEventId || null)}}\n\n`);

          const ping = setInterval(() => {
            try {
              controller.enqueue(new TextEncoder().encode(': ping\n\n'));
            } catch {
              clearInterval(ping);
              sseClients.delete(send);
            }
          }, 15000);
        },
        cancel() {
          // Handled via set deletion on error
        },
      });

      return new Response(stream, {
        status: 200,
        headers: {
          'Content-Type': 'text/event-stream',
          'Cache-Control': 'no-cache',
          'Connection': 'keep-alive',
          'Access-Control-Allow-Origin': '*',
        },
      });
    }

    // Test helper control endpoints
    if (path === '/api/test/reset' && method === 'POST') {
      resetState();
      return new Response(JSON.stringify({ ok: true }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (path === '/api/test/emit-event' && method === 'POST') {
      const body = await req.json().catch(() => ({}));
      broadcastSse(body.type || 'bank.transaction.credit', body.data || {}, body.id);
      return new Response(JSON.stringify({ ok: true, count: sseClients.size }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Mock VNC html
    if (path === '/mock-vnc.html') {
      return new Response(
        '<!DOCTYPE html><html><body><h1>ACB Mock VNC Screen</h1><p>VNC Session Connected</p></body></html>',
        { status: 200, headers: { 'Content-Type': 'text/html; charset=utf-8' } },
      );
    }

    // Static assets & SPA fallback
    let filePath = join(distDir, path);
    if (!existsSync(filePath) || !extname(filePath)) {
      filePath = join(distDir, 'index.html');
    }

    if (existsSync(filePath)) {
      const ext = extname(filePath).toLowerCase();
      const contentType = mimeTypes[ext] || 'application/octet-stream';
      const fileBytes = readFileSync(filePath);
      return new Response(fileBytes, {
        status: 200,
        headers: {
          'Content-Type': contentType,
          'Cache-Control': ext === '.html' ? 'no-cache' : 'max-age=3600',
        },
      });
    }

    return new Response('Not found', { status: 404 });
  },
});

console.log(`[fixture-server] Deterministic fixture server listening on http://127.0.0.1:${server.port}`);
