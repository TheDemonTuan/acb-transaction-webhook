import { describe, it, expect } from 'vitest';
import { computeQRHealthState } from '../src/features/payment-qr/ReceivingQRModal';

describe('QR Modal Health State Computation', () => {
  describe('Public Viewer (isPublic = true)', () => {
    it('evaluates to healthy when transport (internet, gateway, SSE) is healthy, ignoring internal ACB state', () => {
      const res = computeQRHealthState({
        isPublic: true,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'CONNECTED',
        acbState: 'AUTH_REQUIRED', // Even if ACB state happens to be anything
        pollError: { type: 'critical', message: 'ACB session expired' },
      });

      expect(res.isInternetDown).toBe(false);
      expect(res.isGatewayDown).toBe(false);
      expect(res.isSseBroken).toBe(false);
      expect(res.isSseStale).toBe(false);
      // Public viewer MUST NOT consider internal ACB state as critical or warning
      expect(res.isAcbCritical).toBe(false);
      expect(res.isAcbWarning).toBe(false);
      expect(res.hasCritical).toBe(false);
      expect(res.hasWarning).toBe(false);
    });

    it('triggers critical when internet is offline', () => {
      const res = computeQRHealthState({
        isPublic: true,
        networkOnline: false,
        serverReachable: false,
        sseStatus: 'DISCONNECTED',
        acbState: 'UNKNOWN',
        pollError: null,
      });

      expect(res.isInternetDown).toBe(true);
      expect(res.hasCritical).toBe(true);
    });

    it('triggers critical when gateway is unreachable', () => {
      const res = computeQRHealthState({
        isPublic: true,
        networkOnline: true,
        serverReachable: false,
        sseStatus: 'DISCONNECTED',
        acbState: 'UNKNOWN',
        pollError: null,
      });

      expect(res.isGatewayDown).toBe(true);
      expect(res.hasCritical).toBe(true);
    });

    it('triggers critical when SSE is disconnected', () => {
      const res = computeQRHealthState({
        isPublic: true,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'DISCONNECTED',
        acbState: 'UNKNOWN',
        pollError: null,
      });

      expect(res.isSseBroken).toBe(true);
      expect(res.hasCritical).toBe(true);
    });

    it('triggers warning when SSE is reconnecting or stale', () => {
      const resReconnecting = computeQRHealthState({
        isPublic: true,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'RECONNECTING',
        acbState: 'UNKNOWN',
        pollError: null,
      });

      expect(resReconnecting.hasCritical).toBe(false);
      expect(resReconnecting.hasWarning).toBe(true);
      expect(resReconnecting.isSseStale).toBe(true);

      const resStale = computeQRHealthState({
        isPublic: true,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'STALE',
        acbState: 'UNKNOWN',
        pollError: null,
      });

      expect(resStale.hasCritical).toBe(false);
      expect(resStale.hasWarning).toBe(true);
      expect(resStale.isSseStale).toBe(true);
    });
  });

  describe('Admin View (isPublic = false)', () => {
    it('triggers critical when ACB authentication is expired/required', () => {
      const res = computeQRHealthState({
        isPublic: false,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'CONNECTED',
        acbState: 'AUTH_REQUIRED',
        pollError: null,
      });

      expect(res.isAcbCritical).toBe(true);
      expect(res.hasCritical).toBe(true);
    });

    it('triggers critical when poll error is critical', () => {
      const res = computeQRHealthState({
        isPublic: false,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'CONNECTED',
        acbState: 'MONITORING',
        pollError: { type: 'critical', message: 'Session dead' },
      });

      expect(res.isAcbCritical).toBe(true);
      expect(res.hasCritical).toBe(true);
    });

    it('triggers warning when ACB is AUTH_STARTING or poll error is warning', () => {
      const resAuthStarting = computeQRHealthState({
        isPublic: false,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'CONNECTED',
        acbState: 'AUTH_STARTING',
        pollError: null,
      });

      expect(resAuthStarting.isAcbWarning).toBe(true);
      expect(resAuthStarting.hasWarning).toBe(true);
      expect(resAuthStarting.hasCritical).toBe(false);

      const resWarningPoll = computeQRHealthState({
        isPublic: false,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'CONNECTED',
        acbState: 'MONITORING',
        pollError: { type: 'warning', message: 'Rate limited' },
      });

      expect(resWarningPoll.isAcbWarning).toBe(true);
      expect(resWarningPoll.hasWarning).toBe(true);
      expect(resWarningPoll.hasCritical).toBe(false);
    });

    it('evaluates to all healthy when everything is online and ACB is MONITORING', () => {
      const res = computeQRHealthState({
        isPublic: false,
        networkOnline: true,
        serverReachable: true,
        sseStatus: 'CONNECTED',
        acbState: 'MONITORING',
        pollError: null,
      });

      expect(res.hasCritical).toBe(false);
      expect(res.hasWarning).toBe(false);
      expect(res.isAcbCritical).toBe(false);
      expect(res.isAcbWarning).toBe(false);
    });
  });
});
