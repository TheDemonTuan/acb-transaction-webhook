import { describe, expect, it } from 'vitest';
import { formatErrorMessage } from '../src/content/error-copy';

describe('error-copy', () => {
  it('filters out raw HTML pages or DOCTYPE leak', () => {
    const htmlErr = new Error('<!DOCTYPE html><title>Cloudflare 502</title>');
    expect(formatErrorMessage(htmlErr)).toBe('Không thể kết nối máy chủ hoặc phản hồi không hợp lệ. Vui lòng thử lại.');
  });
});
