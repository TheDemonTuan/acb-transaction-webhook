import React, { useEffect } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useRealtimeContext } from './RealtimeProvider';
import { useVoiceAnnouncements } from '../features/voice-announcements/VoiceAnnouncementProvider';
import { queryKeys } from '../shared/api/query-keys';
import type {
  PageResponse,
  Transaction,
} from '../realtime-types';
import type { BankTransactionCreditData, PollCompletedData, RealtimeEnvelope } from './realtime.types';

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

        // Optimistically prepend transaction to matching caches (unfiltered or credit-only, matching date bounds)
        const txDay = data.transactionDay || (data.transactionDate ? data.transactionDate.substring(0, 10) : '');
        const newTx: Transaction = {
          id: data.transactionId,
          semanticKey: `ACB:${data.transactionNumber}`,
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

        // Update aggregate summary on paginated queries (cursor present) without injecting items
        queryClient.setQueriesData<PageResponse<Transaction>>(
          {
            predicate: (query) => {
              const [key, params] = query.queryKey as [string, Record<string, any> | undefined];
              if (key !== 'transactions') return false;
              if (!params?.cursor) return false;
              if (params?.direction && params.direction === 'debit') return false;
              if (params?.from && txDay && txDay < String(params.from)) return false;
              if (params?.to && txDay && txDay > String(params.to)) return false;
              if (params?.query || params?.q) return false;
              return true;
            },
          },
          (old) => {
            if (!old) return old;
            return {
              ...old,
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

        // Invalidate status & overview metrics
        queryClient.invalidateQueries({ queryKey: queryKeys.status });
        queryClient.invalidateQueries({ queryKey: queryKeys.adminOverview });
      }
    );

    // 2. poll.completed -> Invalidate polls, and transactions if insertedCount > 0
    const unsubPoll = subscribe<PollCompletedData>('poll.completed', (envelope) => {
      const data = envelope.data;
      queryClient.invalidateQueries({ queryKey: queryKeys.pollRuns() });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
      queryClient.invalidateQueries({ queryKey: queryKeys.adminOverview });

      if (data && (data.insertedCount ?? 0) > 0) {
        queryClient.invalidateQueries({ queryKey: queryKeys.transactions() });
        queryClient.invalidateQueries({ queryKey: queryKeys.adminOverview });
      }
    });

    // 3. connection.changed & auth.changed -> Invalidate connection and status
    const unsubConn = subscribe('connection.changed', () => {
      queryClient.invalidateQueries({ queryKey: queryKeys.connection });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
    });

    const unsubAuth = subscribe('auth.changed', () => {
      queryClient.invalidateQueries({ queryKey: queryKeys.connection });
      queryClient.invalidateQueries({ queryKey: queryKeys.status });
    });

    // 4. webhook.changed, notification.changed & delivery.changed -> Invalidate webhooks, channels & deliveries
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

    // 5. audit.created -> Invalidate audit logs
    const unsubAudit = subscribe('audit.created', () => {
      queryClient.invalidateQueries({ queryKey: queryKeys.auditLogs() });
    });

    return () => {
      unsubCredit();
      unsubPoll();
      unsubConn();
      unsubAuth();
      unsubWebhook();
      unsubNotification();
      unsubDelivery();
      unsubAudit();
    };
  }, [subscribe, queryClient, handleCreditEvent]);

  return null;
};
