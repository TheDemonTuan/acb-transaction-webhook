import React from 'react';
import { QueryClientProvider } from '@tanstack/react-query';
import { queryClient } from '../shared/api/query-client';
import { RealtimeProvider } from '../realtime/RealtimeProvider';
import { VoiceAnnouncementProvider } from '../features/voice-announcements/VoiceAnnouncementProvider';
import { BankConnectionProvider } from '../features/bank-connection/BankConnectionProvider';
import { RealtimeDomainBridge } from '../realtime/RealtimeDomainBridge';

export const AppProviders: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const refreshSnapshot = () => {
    queryClient.invalidateQueries();
  };

  return (
    <QueryClientProvider client={queryClient}>
      <RealtimeProvider onInitialState={refreshSnapshot} onResetState={refreshSnapshot}>
        <VoiceAnnouncementProvider>
          <BankConnectionProvider>
            <RealtimeDomainBridge />
            {children}
          </BankConnectionProvider>
        </VoiceAnnouncementProvider>
      </RealtimeProvider>
    </QueryClientProvider>
  );
};
