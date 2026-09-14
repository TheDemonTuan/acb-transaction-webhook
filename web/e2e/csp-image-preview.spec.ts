import { expect, test } from '@playwright/test';

test('serves the intended CSP on SPA routes', async ({ request }) => {
  for (const path of ['/', '/admin/activity', '/transactions/example']) {
    const response = await request.get(path);
    expect(response.status()).toBe(200);
    const csp = response.headers()['content-security-policy'];
    expect(csp).toContain("script-src 'self' https://static.cloudflareinsights.com");
    expect(csp).toContain("script-src-elem 'self' https://static.cloudflareinsights.com 'unsafe-inline'");
    expect(csp).toContain("script-src-attr 'none'");
    expect(csp).toContain("connect-src 'self' ws: wss: https://cloudflareinsights.com");
    expect(csp).toContain("img-src 'self' data: blob: https:");
    expect(csp).not.toContain("script-src 'self' 'unsafe-inline'");
  }
});

test('stops a failed Bark icon preview and retries after the URL changes', async ({ page }) => {
  await page.route('https://images.example.test/**', (route) => {
    const isGood = route.request().url().endsWith('/good.png');
    return route.fulfill({
      status: isGood ? 200 : 404,
      contentType: 'image/png',
      body: isGood ? Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64') : '',
    });
  });

  await page.goto('/admin/notifications');
  const advanced = page.getByRole('button', { name: /Tùy chỉnh thông báo/ });
  if (await advanced.isVisible()) await advanced.click();

  const iconInput = page.getByLabel('Icon thông báo (URL ảnh hiển thị trên iPhone)');
  await iconInput.fill('https://images.example.test/bad.png');
  await expect(page.getByRole('img', { name: 'Không tải được icon preview' })).toBeVisible();

  await iconInput.fill('https://images.example.test/good.png');
  await expect(page.getByRole('img', { name: 'Icon preview' })).toBeVisible();
});
