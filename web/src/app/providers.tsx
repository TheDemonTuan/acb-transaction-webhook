import React, { useMemo } from 'react';
import { QueryClientProvider } from '@tanstack/react-query';
import { queryClient } from '../shared/api/query-client';
import { RealtimeProvider } from '../realtime/RealtimeProvider';
import { VoiceAnnouncementProvider } from '../features/voice-announcements/VoiceAnnouncementProvider';
import { TransactionAudioEngine } from '../features/voice-announcements/transaction-audio-engine';
import { BankConnectionProvider } from '../features/bank-connection/BankConnectionProvider';
import { RealtimeDomainBridge } from '../realtime/RealtimeDomainBridge';
import { isPublicViewerHost } from './runtime-mode';

export const AppProviders: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const isPublic = isPublicViewerHost();
  const refreshSnapshot = () => {
    queryClient.invalidateQueries();
  };

  const engine = useMemo(() => new TransactionAudioEngine({ isPublic }), [isPublic]);

  if (isPublic) {
    return (
      <QueryClientProvider client={queryClient}>
        <RealtimeProvider
          url="/api/public/v1/events"
          onInitialState={refreshSnapshot}
          onResetState={refreshSnapshot}
        >
          <VoiceAnnouncementProvider engine={engine}>
            <RealtimeDomainBridge />
            {children}
          </VoiceAnnouncementProvider>
        </RealtimeProvider>
      </QueryClientProvider>
    );
  }

  return (
    <QueryClientProvider client={queryClient}>
      <RealtimeProvider onInitialState={refreshSnapshot} onResetState={refreshSnapshot}>
        <VoiceAnnouncementProvider engine={engine}>
          <BankConnectionProvider>
            <RealtimeDomainBridge />
            {children}
          </BankConnectionProvider>
        </VoiceAnnouncementProvider>
      </RealtimeProvider>
    </QueryClientProvider>
  );
};
