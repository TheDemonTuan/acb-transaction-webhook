import { expect, test } from 'vitest';
import {
  CSRF_CODE_ORIGIN_MISMATCH,
  CSRF_CODE_TOKEN_INVALID,
  apiErrorMessage,
  parseApiError,
} from '../src/api';

test('does not expose an HTML upstream error page', async () => {
  const html = '<!DOCTYPE html><title>502: Bad gateway</title>';
  const response = new Response(html, { status: 502, headers: { 'Content-Type': 'text/html' } });
  const message = await apiErrorMessage(response);
  expect(message).toBe('Máy chủ trả về lỗi HTTP 502. Vui lòng thử lại.');
  expect(message).not.toContain('DOCTYPE');
});

test('uses a JSON API error message', async () => {
  const response = new Response(JSON.stringify({ error: 'Phiên đăng nhập đã hết hạn.' }), {
    status: 410,
    headers: { 'Content-Type': 'application/json' },
  });
  await expect(apiErrorMessage(response)).resolves.toBe('Phiên đăng nhập đã hết hạn.');
});

test('parses ApiError for ORIGIN_MISMATCH and CSRF_TOKEN_INVALID with clear messages', async () => {
  const originResponse = new Response(
    JSON.stringify({
      code: CSRF_CODE_ORIGIN_MISMATCH,
      error: 'csrf validation failed: origin mismatch',
    }),
    { status: 403, headers: { 'Content-Type': 'application/json' } }
  );
  const originError = await parseApiError(originResponse);
  expect(originError.status).toBe(403);
  expect(originError.code).toBe('ORIGIN_MISMATCH');
  expect(originError.message).toContain('PUBLIC_ORIGIN');

  const tokenResponse = new Response(
    JSON.stringify({
      code: CSRF_CODE_TOKEN_INVALID,
      error: 'csrf validation failed: invalid token',
    }),
    { status: 403, headers: { 'Content-Type': 'application/json' } }
  );
  const tokenError = await parseApiError(tokenResponse);
  expect(tokenError.status).toBe(403);
  expect(tokenError.code).toBe('CSRF_TOKEN_INVALID');
  expect(tokenError.message).toContain('CSRF');
});
