import { describe, it, expect } from 'bun:test';
import { queryKeys } from '../src/shared/api/query-keys';
import type { NotificationChannel, NotificationProvider, BarkConfig } from '../src/realtime-types';

describe('Notification Channels & Providers Types and Keys', () => {
  it('defines consistent query keys for notification channels and providers', () => {
    expect(queryKeys.notificationChannels).toEqual(['notification-channels']);
    expect(queryKeys.notificationProviders).toEqual(['notification-providers']);
    expect(queryKeys.webhooks).toEqual(['webhooks']);
  });

  it('validates shape of BarkConfig and NotificationChannel', () => {
    const config: BarkConfig = {
      group: 'ACB',
      level: 'timeSensitive',
      sound: 'shake',
      includeBalance: true,
      includeDescription: true,
      dashboardLink: true,
    };

    const channel: NotificationChannel = {
      id: 'ch_bark_1',
      name: 'iPhone 16 Pro',
      provider: 'BARK',
      status: 'ACTIVE',
      revision: 1,
      barkConfig: config,
      hasDeviceKey: true,
      createdAt: '2026-09-13T00:00:00Z',
      updatedAt: '2026-09-13T00:00:00Z',
    };

    expect(channel.provider).toBe('BARK');
    expect(channel.hasDeviceKey).toBe(true);
    expect(channel.secret).toBeUndefined();
    expect(channel.barkConfig?.level).toBe('timeSensitive');
  });

  it('validates shape of NotificationProvider', () => {
    const provider: NotificationProvider = {
      id: 'BARK',
      name: 'Bark (iOS)',
      description: 'Đẩy thông báo trực tiếp tới iPhone qua Bark self-host.',
      configured: true,
      publicUrl: 'https://push.example.com',
    };

    expect(provider.id).toBe('BARK');
    expect(provider.configured).toBe(true);
    expect(provider.publicUrl).toBe('https://push.example.com');
  });
});
