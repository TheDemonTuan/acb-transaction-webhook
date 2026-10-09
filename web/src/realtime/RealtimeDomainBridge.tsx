import React, { useEffect } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useRealtimeContext } from './RealtimeProvider';
import { useVoiceAnnouncements } from '../features/voice-announcements/VoiceAnnouncementProvider';
import { queryKeys } from '../shared/api/query-keys';
import type {
  PageResponse,
  Transaction,
} from '../realtime-types';
import type { BankTransactionCreditData, RealtimeEnvelope } from './realtime.types';
import { creditTransactionKey } from './realtime.events';
import { creditMatchesPaymentOrder } from '../features/payment-qr/payment-orders';

export const RealtimeDomainBridge: React.FC = () => {
  const queryClient = useQueryClient();
  const { subscribe } = useRealtimeContext();
  const { handleCreditEvent } = useVoiceAnnouncements();

  useEffect(() => {
    // 1. bank.transaction.credit -> Update cache & trigger voice
    const unsubCredit = subscribe<BankTransactionCreditData>(
      'bank.transaction.credit',
      async (envelope: RealtimeEnvelope<BankTransactionCreditData>) => {
        const data = envelope.data;
        if (!data) return;

        // Trigger voice announcement
        handleCreditEvent(envelope);

        // Credit is a refetch signal, never authority to mark an order PAID.
        if (data.provider === 'PAYOS' && data.orderCode) {
          queryClient.invalidateQueries({ queryKey: queryKeys.payments() });
          queryClient.invalidateQueries({
            predicate: (query) => {
              if (query.queryKey[0] !== 'payment-order' && query.queryKey[0] !== 'public-payment-order') return false;
              const snapshot = query.state.data;
              // A not-yet-loaded snapshot may be the correlated order.
              if (!snapshot || typeof snapshot !== 'object' || !('orderCode' in snapshot)) return true;
              return typeof snapshot.orderCode === 'string' &&
                creditMatchesPaymentOrder({ orderCode: snapshot.orderCode }, data);
            },
          });
        }

        // Optimistically prepend transaction to matching caches (unfiltered or credit-only, matching date bounds)
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

        // Optimistically prepend transaction ONLY to the first page (no cursor) to avoid mixing across pages
        queryClient.setQueriesData<PageResponse<Transaction>>(
          {
            predicate: (query) => {
              const [key, params] = query.queryKey as [string, Record<string, any> | undefined];
              if (key !== 'transactions') return false;
              if (params?.cursor) return false;
              if (params?.direction && params.direction === 'debit') return false;
              if (params?.from && txDay && txDay < String(params.from)) return false;
              if (params?.to && txDay && txDay > String(params.to)) return false;
              if (params?.query || params?.q) return false;
              return true;
            },
          },
          (old) => {
            if (!old) {
              return { items: [newTx] };
            }
            const currentItems = Array.isArray(old.items) ? old.items : [];
            if (currentItems.some((t) => t.id === newTx.id || t.semanticKey === newTx.semanticKey)) {
              return old;
            }
            return {
              ...old,
              items: [newTx, ...currentItems],
              summary: old.summary
                ? {
                    ...old.summary,
                    count: (old.summary.count || 0) + 1,
                    incoming: (old.summary.incoming || 0) + (newTx.credit || 0),
                  }
                : undefined,
            };
          }
        );

        // Refetch committed history/summary for every source, including CATCH_UP.
        // In particular, do not increment paginated totals on a journal replay.
        queryClient.invalidateQueries({ queryKey: queryKeys.transactions() });

        // Invalidate status & overview metrics
        queryClient.invalidateQueries({ queryKey: queryKeys.status });
        queryClient.invalidateQueries({ queryKey: queryKeys.adminOverview });
      }
    );


    // Webhook, notification and delivery changes refresh their operational views.
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

    // Audit entries refresh the audit log.
    const unsubAudit = subscribe('audit.created', () => {
      queryClient.invalidateQueries({ queryKey: queryKeys.auditLogs() });
    });

    return () => {
      unsubCredit();
      unsubWebhook();
      unsubNotification();
      unsubDelivery();
      unsubAudit();
    };
  }, [subscribe, queryClient, handleCreditEvent]);

  return null;
};
