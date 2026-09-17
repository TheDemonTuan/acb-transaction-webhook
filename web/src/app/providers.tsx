import React, { useMemo } from 'react';
import { QueryClientProvider } from '@tanstack/react-query';
import { queryClient } from '../shared/api/query-client';
import { RealtimeProvider } from '../realtime/RealtimeProvider';
import { VoiceAnnouncementProvider } from '../features/voice-announcements/VoiceAnnouncementProvider';
import { BrowserSpeechEngine } from '../features/voice-announcements/browser-speech-engine';
import { BankConnectionProvider } from '../features/bank-connection/BankConnectionProvider';
import { RealtimeDomainBridge } from '../realtime/RealtimeDomainBridge';
import { isPublicViewerHost } from './runtime-mode';

export const AppProviders: React.FC<{ children: React.ReactNode }> = ({ children }) => {
  const isPublic = isPublicViewerHost();
  const refreshSnapshot = () => {
    queryClient.invalidateQueries();
  };

  const browserEngine = useMemo(() => (isPublic ? new BrowserSpeechEngine() : undefined), [isPublic]);

  if (isPublic) {
    return (
      <QueryClientProvider client={queryClient}>
        <RealtimeProvider
          url="/api/public/v1/events"
          onInitialState={refreshSnapshot}
          onResetState={refreshSnapshot}
        >
          <VoiceAnnouncementProvider engine={browserEngine}>
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
