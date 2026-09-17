import { describe, it, expect } from 'vitest';
import { shouldRetryQuery } from './query-client';
import { ApiError } from '../../api';

describe('shouldRetryQuery', () => {
  it('does not retry 4xx ApiErrors regardless of failure count', () => {
    const statuses = [400, 401, 403, 404, 405, 429];
    for (const status of statuses) {
      const err = new ApiError('Client Error', status);
      expect(shouldRetryQuery(0, err)).toBe(false);
      expect(shouldRetryQuery(1, err)).toBe(false);
    }
  });

  it('retries once on 5xx server errors', () => {
    const statuses = [500, 502, 503, 504];
    for (const status of statuses) {
      const err = new ApiError('Server Error', status);
      expect(shouldRetryQuery(0, err)).toBe(true);
      expect(shouldRetryQuery(1, err)).toBe(false);
    }
  });

  it('retries once on network/unknown errors', () => {
    const networkErr = new Error('Failed to fetch');
    expect(shouldRetryQuery(0, networkErr)).toBe(true);
    expect(shouldRetryQuery(1, networkErr)).toBe(false);
  });
});
