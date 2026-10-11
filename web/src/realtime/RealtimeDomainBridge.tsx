import React, { useEffect, useRef } from 'react';
import { useQueryClient, QueryClient } from '@tanstack/react-query';
import { useRealtimeContext } from './RealtimeProvider';
import { useVoiceAnnouncements } from '../features/voice-announcements/VoiceAnnouncementProvider';
import { queryKeys } from '../shared/api/query-keys';
import type { PageResponse, Transaction } from '../realtime-types';
import type { BankTransactionCreditData, RealtimeEnvelope } from './realtime.types';
import { creditTransactionKey } from './realtime.events';
import { creditMatchesPaymentOrder } from '../features/payment-qr/payment-orders';

export interface DirtyInvalidationState {
  transactions: boolean;
  status: boolean;
  adminOverview: boolean;
  sepayStore: boolean;
  payments: boolean;
  orderCodes: Set<string>;
}

export class RealtimeInvalidationCoalescer {
  private queryClient: QueryClient;
  private dirty: DirtyInvalidationState = {
    transactions: false,
    status: false,
    adminOverview: false,
    sepayStore: false,
    payments: false,
    orderCodes: new Set(),
  };
  private timer: number | null = null;

  constructor(queryClient: QueryClient) {
    this.queryClient = queryClient;
  }

  markCredit(data: BankTransactionCreditData): void {
    this.dirty.transactions = true;
    this.dirty.status = true;
    this.dirty.adminOverview = true;
    if (data.provider === 'SEPAY') {
      this.dirty.sepayStore = true;
    }
    if (data.provider === 'PAYOS' && data.orderCode) {
      this.dirty.payments = true;
      this.dirty.orderCodes.add(String(data.orderCode));
    }
    this.schedule();
  }

  schedule(): void {
    if (this.timer !== null) return;
    this.timer = setTimeout(() => {
      this.flush();
    }, 250) as unknown as number;
  }

  flush(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
    const current = {
      ...this.dirty,
      orderCodes: new Set(this.dirty.orderCodes),
    };
    this.dirty = {
      transactions: false,
      status: false,
      adminOverview: false,
      sepayStore: false,
      payments: false,
      orderCodes: new Set(),
    };

    if (current.transactions) this.queryClient.invalidateQueries({ queryKey: queryKeys.transactions() });
    if (current.status) this.queryClient.invalidateQueries({ queryKey: queryKeys.status });
    if (current.adminOverview) this.queryClient.invalidateQueries({ queryKey: queryKeys.adminOverview });
    if (current.sepayStore) this.queryClient.invalidateQueries({ queryKey: queryKeys.sepayStore });
    if (current.payments) this.queryClient.invalidateQueries({ queryKey: queryKeys.payments() });
    if (current.orderCodes.size > 0) {
      this.queryClient.invalidateQueries({
        predicate: (query) => {
          if (query.queryKey[0] !== 'payment-order' && query.queryKey[0] !== 'public-payment-order') return false;
          const snapshot = query.state.data;
          if (!snapshot || typeof snapshot !== 'object' || !('orderCode' in snapshot)) return true;
          return typeof snapshot.orderCode === 'string' && current.orderCodes.has(snapshot.orderCode);
        },
      });
    }
  }

  cancel(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }
}

export async function applyCreditEventToQueryCache(
  queryClient: QueryClient,
  data: BankTransactionCreditData
): Promise<void> {
  const txDay = data.transactionDay || (data.transactionDate ? data.transactionDate.substring(0, 10) : '');
  const newTx: Transaction = {
    id: data.transactionId,
    bank: data.bank,
    provider: data.provider,
    orderCode: data.orderCode,
    semanticKey: creditTransactionKey(data) || data.transactionId,
    transactionDate: data.transactionDate,
    transactionDay: data.transactionDay,
    datePrecision: data.datePrecision,
    effectiveDate: data.transactionDate,
    debit: Number(data.debit || 0),
    credit: Number(data.credit || 0),
    description: data.description || '',
    firstSeenAt: data.detectedAt || new Date().toISOString(),
    source: data.source || 'REALTIME',
  };

  // Stop an in-flight snapshot request from overwriting this realtime row.
  await queryClient.cancelQueries({ queryKey: queryKeys.transactions() });

  // Optimistically prepend transaction ONLY to matching first-page caches (no cursor)
  const matches = queryClient.getQueriesData<PageResponse<Transaction>>({
    predicate: (query) => {
      const [key, params] = query.queryKey as [string, Record<string, unknown> | undefined];
      if (key !== 'transactions') return false;
      if (params?.cursor) return false;
      if (params?.direction && params.direction === 'debit') return false;
      if (params?.from && txDay && txDay < String(params.from)) return false;
      if (params?.to && txDay && txDay > String(params.to)) return false;
      if (params?.query || params?.q) return false;
      return true;
    },
  });

  for (const [queryKey, old] of matches) {
    if (!old) continue;
    const currentItems = Array.isArray(old.items) ? old.items : [];
    if (currentItems.some((t) => t.id === newTx.id || t.semanticKey === newTx.semanticKey)) {
      continue;
    }
    const [, params] = queryKey as [string, Record<string, unknown> | undefined];
    const rawLimit = typeof params?.limit === 'number' ? params.limit : 50;
    const limit = Math.min(100, Math.max(1, rawLimit));
    const nextItems = [newTx, ...currentItems].slice(0, limit);
    queryClient.setQueryData(queryKey, {
      ...old,
      items: nextItems,
    });
  }
}

export const RealtimeDomainBridge: React.FC = () => {
  const queryClient = useQueryClient();
  const { subscribe, reconnectCount, status } = useRealtimeContext();
  const { handleCreditEvent, refreshProfile } = useVoiceAnnouncements();
  const coalescerRef = useRef<RealtimeInvalidationCoalescer | null>(null);

  if (!coalescerRef.current) {
    coalescerRef.current = new RealtimeInvalidationCoalescer(queryClient);
  }

  const lastReconnect = useRef(reconnectCount);
  useEffect(() => {
    if (status === 'CONNECTED' && reconnectCount !== lastReconnect.current) {
      lastReconnect.current = reconnectCount;
      refreshProfile();
    }
  }, [reconnectCount, status, refreshProfile]);

  useEffect(() => {
    const coalescer = coalescerRef.current;

    const unsubCredit = subscribe<BankTransactionCreditData>(
      'bank.transaction.credit',
      async (envelope: RealtimeEnvelope<BankTransactionCreditData>) => {
        const data = envelope.data;
        if (!data) return;

        // Trigger voice announcement / flash immediately
        handleCreditEvent(envelope);

        // Schedule coalesced dirty query invalidation
        if (coalescer) {
          coalescer.markCredit(data);
        }

        // Apply optimistic cache update
        await applyCreditEventToQueryCache(queryClient, data);
      }
    );

    const unsubWebhook = subscribe('webhook.changed', () => {
      queryClient.invalidateQueries({ queryKey: queryKeys.webhooks });
      queryClient.invalidateQueries({ queryKey: queryKeys.notificationChannels });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
    });

    const unsubNotification = subscribe('notification.changed', () => {
      queryClient.invalidateQueries({ queryKey: queryKeys.notificationChannels });
      queryClient.invalidateQueries({ queryKey: queryKeys.webhooks });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
    });

    const unsubDelivery = subscribe('delivery.changed', () => {
      queryClient.invalidateQueries({ queryKey: queryKeys.deliveries() });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
    });

    const unsubAudit = subscribe('audit.created', () => {
      queryClient.invalidateQueries({ queryKey: queryKeys.auditLogs() });
    });

    const unsubVoiceSettings = subscribe<{ revision?: number }>('voice.settings.changed', (envelope) => {
      if (envelope.data?.revision !== undefined) refreshProfile(envelope.data.revision);
    });

    return () => {
      if (coalescer) {
        coalescer.cancel();
      }
      unsubCredit();
      unsubWebhook();
      unsubNotification();
      unsubDelivery();
      unsubAudit();
      unsubVoiceSettings();
    };
  }, [subscribe, queryClient, handleCreditEvent, refreshProfile]);

  return null;
};
