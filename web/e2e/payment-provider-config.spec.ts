import { expect, test, type Page } from '@playwright/test';
import type { PaymentProviderConfig, PaymentProviderConfigInput } from '../src/features/payment-qr/payment-orders';

type Role = 'OWNER' | 'OPERATOR' | 'VIEWER';

async function installProviderFixture(page: Page, role: Role = 'OWNER', configured = false) {
  let config: PaymentProviderConfig = {
    configured,
    ...(configured ? { clientId: 'owner-only-channel' } : {}),
    apiKeyConfigured: configured,
    checksumKeyConfigured: configured,
    webhookConfirmed: false,
    enabled: false,
    staticUrl: 'http://127.0.0.1:5173/pay',
    webhookUrl: 'http://127.0.0.1:5173/api/integrations/payos/webhook',
  };
  const saves: PaymentProviderConfigInput[] = [];
  let configReads = 0;
  let confirms = 0;
  let rejectConfirm = false;
  let rejectSave = false;
  let statusReads = 0;
  let publicConfigReads = 0;
  const readiness = () => !config.configured ? 'UNCONFIGURED' : !config.enabled ? 'DISABLED' : !config.webhookConfirmed ? 'WEBHOOK_UNCONFIRMED' : 'READY';
  await page.route(/\/api\/v1\/status$/, (route) => {
    statusReads += 1;
    return route.fulfill({ json: {
      service: 'fixture', version: 'fixture', uptimeSeconds: 1, userRole: role,
      payments: { provider: 'PAYOS', bank: 'KienlongBank', configured: config.configured, status: readiness(), webhookConfirmed: config.webhookConfirmed, lastWebhookAt: null, lastReconciledAt: null, pendingOrders: 0, reviewCount: 0 },
      storage: { status: 'ok' }, webhooks: { pending: 0, deadLetter: 0 },
    } });
  });
  await page.route(/\/api\/public\/v1\/payment-config$/, (route) => {
    publicConfigReads += 1;
    return route.fulfill({ json: { provider: 'PAYOS', bank: 'KienlongBank', staticUrl: config.staticUrl, minAmountVnd: 1, maxAmountVnd: 500_000_000, ready: readiness() === 'READY', status: readiness() } });
  });
  await page.route(/\/api\/v1\/payment-provider\/config$/, async (route) => {
    if (route.request().method() === 'GET') {
      configReads += 1;
      return route.fulfill({ status: role === 'OWNER' ? 200 : 403, json: role === 'OWNER' ? config : { error: 'Forbidden' } });
    }
    expect(role).toBe('OWNER');
    expect(route.request().headers()['x-csrf-token']).toBe('provider-csrf');
    const body = route.request().postDataJSON() as PaymentProviderConfigInput;
    saves.push(body);
    if (rejectSave) return route.fulfill({ status: 409, json: { code: 'PROVIDER_CONFIG_BUSY', error: body.apiKey } });
    config = {
      ...config,
      configured: true, clientId: body.clientId, apiKeyConfigured: true, checksumKeyConfigured: true, enabled: body.enabled,
      webhookConfirmed: body.apiKey || body.checksumKey || body.clientId !== config.clientId ? false : config.webhookConfirmed,
    };
    return route.fulfill({ json: config });
  });
  await page.route(/\/api\/v1\/payment-provider\/confirm-webhook$/, (route) => {
    expect(role).toBe('OWNER');
    expect(route.request().headers()['x-csrf-token']).toBe('provider-csrf');
    expect(route.request().postData()).toBeNull();
    confirms += 1;
    if (rejectConfirm) return route.fulfill({ status: 503, json: { error: 'Provider unavailable', code: 'PAYMENT_UNAVAILABLE' } });
    config = { ...config, webhookConfirmed: true };
    return route.fulfill({ json: { confirmed: true } });
  });
  await page.route(/\/api\/v1\/csrf$/, (route) => route.fulfill({ json: { token: 'provider-csrf' } }));
  await page.route(/\/api\/v1\/(?:payments|payment-reviews)(?:\?.*)?$/, (route) => route.fulfill({ json: { items: [] } }));
  return {
    saves,
    get config() { return config; },
    get configReads() { return configReads; },
    get confirms() { return confirms; },
    get statusReads() { return statusReads; },
    get publicConfigReads() { return publicConfigReads; },
    failConfirmation(value: boolean) { rejectConfirm = value; },
    failSave(value: boolean) { rejectSave = value; },
  };
}

const fields = (page: Page) => ({
  clientId: page.locator('input[name="clientId"]'),
  apiKey: page.locator('input[name="apiKey"]'),
  checksumKey: page.locator('input[name="checksumKey"]'),
  enabled: page.getByRole('checkbox', { name: 'Bật nhận đơn mới' }),
  save: page.getByRole('button', { name: 'Lưu cấu hình payOS' }),
  confirm: page.getByRole('button', { name: 'Xác nhận webhook', exact: true }),
});

async function expectSecretsAbsentFromStorage(page: Page, secrets: string[]) {
  const stored = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }));
  for (const secret of secrets) expect(stored).not.toContain(secret);
}

test.describe('Owner-managed payOS channel', () => {
  test('saves write-only keys, refreshes readiness, and requires a successful owner confirmation', async ({ page }) => {
    const fixture = await installProviderFixture(page);
    await page.goto('/admin/connection');
    const form = fields(page);
    await expect(form.clientId).toBeVisible();
    for (const input of [form.clientId, form.apiKey, form.checksumKey]) await expect(input).toHaveAttribute('type', 'password');
    await expect(form.confirm).toBeDisabled();
    await form.clientId.fill('owner-only-channel');
    await form.apiKey.fill('api-secret-not-for-storage');
    await form.checksumKey.fill('checksum-secret-not-for-storage');
    await form.enabled.check();
    const beforeSave = { status: fixture.statusReads, public: fixture.publicConfigReads };
    await form.save.click();
    await expect(form.apiKey).toHaveValue('');
    await expect(form.checksumKey).toHaveValue('');
    expect(fixture.saves).toEqual([{ clientId: 'owner-only-channel', apiKey: 'api-secret-not-for-storage', checksumKey: 'checksum-secret-not-for-storage', enabled: true }]);
    await expect.poll(() => fixture.statusReads).toBeGreaterThan(beforeSave.status);
    await expect.poll(() => fixture.publicConfigReads).toBeGreaterThan(beforeSave.public);
    await expect(form.confirm).toBeEnabled();
    expect(fixture.config.webhookConfirmed).toBe(false);
    fixture.failConfirmation(true);
    await form.confirm.click();
    await expect(page.getByRole('alert')).toBeVisible();
    expect(fixture.config.webhookConfirmed).toBe(false);
    fixture.failConfirmation(false);
    const beforeConfirm = fixture.publicConfigReads;
    await form.confirm.click();
    await expect.poll(() => fixture.config.webhookConfirmed).toBe(true);
    await expect.poll(() => fixture.publicConfigReads).toBeGreaterThan(beforeConfirm);
    expect(fixture.confirms).toBe(2);
    await expectSecretsAbsentFromStorage(page, ['api-secret-not-for-storage', 'checksum-secret-not-for-storage']);
    await page.reload();
    await expect(form.apiKey).toHaveValue('');
    await expect(form.checksumKey).toHaveValue('');
    expect(fixture.saves).toHaveLength(1);
    await form.apiKey.fill('rotated-api-not-for-storage');
    await expect(form.confirm).toBeDisabled();
    await form.save.click();
    await expect(form.apiKey).toHaveValue('');
    expect(fixture.config.webhookConfirmed).toBe(false);
    expect(fixture.saves).toHaveLength(2);
    expect(fixture.saves[1]).toEqual({ clientId: 'owner-only-channel', apiKey: 'rotated-api-not-for-storage', checksumKey: '', enabled: true });
    await expect(form.confirm).toBeEnabled();
    await expectSecretsAbsentFromStorage(page, ['rotated-api-not-for-storage']);
  });

  test('keeps existing keys on an enabled-only save and requires both keys for a different client', async ({ page }) => {
    const fixture = await installProviderFixture(page, 'OWNER', true);
    await page.goto('/admin/connection');
    const form = fields(page);
    await expect(form.clientId).toHaveValue('owner-only-channel');
    await expect(form.apiKey).not.toHaveAttribute('required');
    await form.enabled.check();
    await form.save.click();
    await expect.poll(() => fixture.saves.length).toBe(1);
    expect(fixture.saves[0]).toEqual({ clientId: 'owner-only-channel', apiKey: '', checksumKey: '', enabled: true });
    await form.clientId.fill('different-channel');
    await expect(form.apiKey).toHaveAttribute('required', '');
    await expect(form.checksumKey).toHaveAttribute('required', '');
    await expect(form.confirm).toBeDisabled();
    await form.save.click();
    expect(fixture.saves).toHaveLength(1);
  });

  test('does not render provider errors containing keys and retains inputs for an explicit retry', async ({ page }) => {
    const fixture = await installProviderFixture(page, 'OWNER', true);
    fixture.failSave(true);
    await page.goto('/admin/connection');
    const form = fields(page);
    await form.apiKey.fill('never-render-this-api-key');
    await form.save.click();
    await expect(page.getByRole('alert')).toBeVisible();
    expect(await page.locator('body').innerText()).not.toContain('never-render-this-api-key');
    await expect(form.apiKey).toHaveValue('never-render-this-api-key');
    await expectSecretsAbsentFromStorage(page, ['never-render-this-api-key']);
    fixture.failSave(false);
    await form.save.click();
    await expect(form.apiKey).toHaveValue('');
    expect(fixture.saves).toHaveLength(2);
  });

  for (const role of ['OPERATOR', 'VIEWER'] as const) {
    test(`${role} sees safe status and canonical URLs without fetching or editing owner configuration`, async ({ page }) => {
      const fixture = await installProviderFixture(page, role, true);
      await page.goto('/admin/connection');
      await expect(page.getByRole('link', { name: fixture.config.staticUrl, exact: true }).first()).toHaveAttribute('href', fixture.config.staticUrl);
      await expect(page.getByText(fixture.config.webhookUrl, { exact: true })).toBeVisible();
      const form = fields(page);
      await expect(form.clientId).toHaveCount(0);
      await expect(form.apiKey).toHaveCount(0);
      await expect(form.checksumKey).toHaveCount(0);
      await expect(form.enabled).toHaveCount(0);
      await expect(form.save).toHaveCount(0);
      await expect(form.confirm).toHaveCount(0);
      expect(await page.locator('body').innerText()).not.toContain('owner-only-channel');
      expect(fixture.configReads).toBe(0);
      expect(fixture.saves).toHaveLength(0);
      expect(fixture.confirms).toBe(0);
    });
  }
});
