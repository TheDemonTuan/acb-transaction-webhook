import { expect, test, type Page } from '@playwright/test';
import QRCode from 'qrcode';
import type { SePayAdminConfig, SePayAdminConfigInput, SePayAdminFields } from '../src/features/bank-connection/sepay-admin';

const transferPayload = () => {
  const field = (tag: string, value: string) => `${tag}${String(value.length).padStart(2, '0')}${value}`;
  const merchant = field('00', 'A000000727') + field('01', field('00', '970436') + field('01', '0000000000')) + field('02', 'QRIBFTTA');
  const raw = field('00', '01') + field('01', '11') + field('38', merchant) + field('53', '704') + field('58', 'VN') + '6304';
  let crc = 0xffff;
  for (const byte of new TextEncoder().encode(raw)) {
    crc ^= byte << 8;
    for (let bit = 0; bit < 8; bit++) crc = crc & 0x8000 ? ((crc << 1) ^ 0x1021) & 0xffff : (crc << 1) & 0xffff;
  }
  return raw + crc.toString(16).toUpperCase().padStart(4, '0');
};
const blank: SePayAdminFields = { mode: 'disabled', storeKey: '', storeName: '', bankCode: '', bankName: '', accountNumber: '', accountName: '', qrPayload: '', botId: '', chatId: '', senderBotId: '', topicId: '0', activationAt: '', receiverVerified: false, sourceSeparated: false };

async function fixture(page: Page, role = 'OWNER') {
  let config: SePayAdminConfig = { revision: 0, config: { ...blank }, hasBotToken: false, hasWebhookSecret: false, lastMessageAt: null, reviewCount: 0 };
  const saves: SePayAdminConfigInput[] = [];
  let reads = 0;
  let registrations = 0;
  let checks = 0;
  let failSave = false;
  let failRead = false;
  let conflict = false;
  await page.route(/\/api\/v1\/status$/, route => route.fulfill({ json: { service: 'fixture', version: 'fixture', uptimeSeconds: 1, userRole: role, storage: { status: 'ok' }, webhooks: { pending: 0, deadLetter: 0 }, sepay: { mode: config.config.mode, lastMessageAt: null, reviewCount: 0 }, payments: { provider: 'PAYOS', configured: false, status: 'UNCONFIGURED', webhookConfirmed: false, lastWebhookAt: null, lastReconciledAt: null, pendingOrders: 0, reviewCount: 0 } } }));
  await page.route(/\/api\/public\/v1\/payment-config$/, route => route.fulfill({ json: { provider: 'PAYOS', bank: 'KienlongBank', ready: false, status: 'UNCONFIGURED', staticUrl: 'http://127.0.0.1:5173/pay', minAmountVnd: 1, maxAmountVnd: 500000000 } }));
  await page.route(/\/api\/v1\/payment-provider\/config$/, route => route.fulfill({ json: { configured: false, apiKeyConfigured: false, checksumKeyConfigured: false, webhookConfirmed: false, enabled: false } }));
  await page.route(/\/api\/v1\/(?:payments|payment-reviews|sepay-reviews)(?:\?.*)?$/, route => route.fulfill({ json: { items: [] } }));
  await page.route(/\/api\/v1\/csrf$/, route => route.fulfill({ json: { token: 'sepay-csrf' } }));
  await page.route(/\/api\/v1\/sepay-store\/config$/, async route => {
    if (route.request().method() === 'GET') { reads++; return route.fulfill({ status: failRead ? 503 : role === 'OWNER' ? 200 : 403, json: failRead ? { error: 'SEPAY_UNAVAILABLE' } : role === 'OWNER' ? config : { error: 'Forbidden' } }); }
    expect(role).toBe('OWNER');
    expect(route.request().headers()['x-csrf-token']).toBe('sepay-csrf');
    const input = route.request().postDataJSON() as SePayAdminConfigInput;
    saves.push(input);
    if (failSave) return route.fulfill({ status: 502, json: { error: input.botToken } });
    if (conflict) return route.fulfill({ status: 409, json: { error: 'SEPAY_REVISION_CONFLICT' } });
    expect(input.revision).toBe(config.revision);
    config = { ...config, revision: config.revision + 1, config: input.config, hasBotToken: config.hasBotToken || Boolean(input.botToken), hasWebhookSecret: true };
    return route.fulfill({ json: config });
  });
  await page.route(/\/api\/v1\/sepay-store\/telegram\/(?:register|status)$/, route => {
    expect(role).toBe('OWNER');
    if (route.request().method() === 'POST') { registrations++; expect(route.request().headers()['x-csrf-token']).toBe('sepay-csrf'); expect(route.request().postData()).toBeNull(); } else checks++;
    return route.fulfill({ json: { registeredUrl: 'https://cashier.example.test/api/integrations/sepay/telegram', pendingUpdateCount: 0, lastErrorAt: null, lastErrorMessage: '', botVerified: true } });
  });
  return { saves, get config() { return config; }, get reads() { return reads; }, get registrations() { return registrations; }, get checks() { return checks; }, fail(value: boolean) { failSave = value; }, failReads(value: boolean) { failRead = value; }, conflict(value: boolean) { conflict = value; } };
}
const field = (page: Page, name: string) => page.locator(`[name="sepay-${name}"]`);
const section = (page: Page) => page.locator('#sepay-store-settings');
async function upload(page: Page, payload: string) {
  await page.getByTestId('sepay-qr-upload').setInputFiles({ name: 'store.png', mimeType: 'image/png', buffer: await QRCode.toBuffer(payload, { width: 640, margin: 4 }) });
}

test('owner decodes locally without BarcodeDetector, saves exact payload and string IDs, activates and registers write-only token', async ({ page }) => {
  const api = await fixture(page);
  await page.addInitScript(() => { Object.defineProperty(window, 'BarcodeDetector', { value: undefined, configurable: true }); });
  await page.goto('/admin/connection');
  await expect(page.getByTestId('sepay-admin-form')).toBeVisible();
  await upload(page, transferPayload());
  await expect(page.getByTestId('sepay-decoded-receiver')).toContainText('0000000000');
  await expect(field(page, 'accountNumber')).toHaveValue('0000000000');
  await expect(field(page, 'bankCode')).toHaveValue('VCB');
  await expect(page.getByRole('img', { name: 'Xem trước QR SePay Store đã giải mã' })).toBeVisible();
  for (const [name, value] of Object.entries({ storeKey: 'fixture-store', storeName: 'Fixture Store', bankName: 'Vietcombank', accountName: 'FIXTURE RECEIVER', botId: '9007199254740993', chatId: '-100900003', senderBotId: '900002', topicId: '0' })) await field(page, name).fill(value);
  await field(page, 'botToken').fill('test-write-only-token');
  await expect(field(page, 'botToken')).toHaveAttribute('type', 'password');
  await expect(section(page).getByRole('button', { name: 'Lưu & bật SePay', exact: true })).toBeDisabled();
  await field(page, 'receiverVerified').check();
  await field(page, 'sourceSeparated').check();
  await section(page).getByRole('button', { name: 'Lưu & bật SePay', exact: true }).click();
  await expect.poll(() => api.saves.length).toBe(1);
  expect(api.saves[0].config.qrPayload).toBe(transferPayload());
  expect(api.saves[0].config.botId).toBe('9007199254740993');
  expect(api.saves[0].config.mode).toBe('active');
  expect(JSON.stringify(api.saves[0])).not.toContain('data:image');
  expect(Object.keys(api.saves[0]).sort()).toEqual(['botToken', 'config', 'revision']);
  await expect(field(page, 'botToken')).toHaveValue('');
  await section(page).getByRole('button', { name: 'Đăng ký webhook Telegram' }).click();
  await expect(page.getByTestId('sepay-telegram-diagnostics')).toBeVisible();
  await section(page).getByRole('button', { name: 'Kiểm tra kết nối Telegram' }).click();
  await expect.poll(() => api.checks).toBe(1);
  expect(api.registrations).toBe(1);
  await section(page).getByRole('button', { name: 'Lưu & tiếp tục nhận SePay' }).click();
  await expect.poll(() => api.saves.length).toBe(2);
  expect(api.saves[1].botToken).toBe('');
  const storage = await page.evaluate(() => JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } }));
  expect(storage).not.toContain('test-write-only-token');
  expect(await page.locator('body').innerText()).not.toContain('test-write-only-token');
  await page.reload();
  await expect(field(page, 'botToken')).toHaveValue('');
  await expect(field(page, 'accountNumber')).toHaveValue('0000000000');
});

test('invalid URL, multi-QR and invalid files cannot replace the decoded transfer payload', async ({ page }) => {
  const api = await fixture(page);
  await page.goto('/admin/connection');
  await upload(page, transferPayload());
  await expect(field(page, 'accountNumber')).toHaveValue('0000000000');
  await upload(page, 'https://example.test/pay');
  await expect(section(page).getByRole('alert')).toBeVisible();
  await expect(field(page, 'accountNumber')).toHaveValue('0000000000');
  const first = await QRCode.toDataURL(transferPayload(), { width: 320, margin: 4 });
  const second = await QRCode.toDataURL('https://example.test/pay', { width: 320, margin: 4 });
  const multiple = await page.evaluate(async ([one, two]) => {
    const canvas = document.createElement('canvas'); canvas.width = 680; canvas.height = 340;
    const ctx = canvas.getContext('2d')!; ctx.fillStyle = '#fff'; ctx.fillRect(0, 0, 680, 340);
    for (const [index, url] of [one, two].entries()) { const image = new Image(); image.src = url; await image.decode(); ctx.drawImage(image, index * 340 + 10, 10); }
    return canvas.toDataURL('image/png').split(',')[1];
  }, [first, second]);
  await page.getByTestId('sepay-qr-upload').setInputFiles({ name: 'multiple.png', mimeType: 'image/png', buffer: Buffer.from(multiple, 'base64') });
  await expect(section(page).getByRole('alert')).toBeVisible();
  await page.getByTestId('sepay-qr-upload').setInputFiles({ name: 'unsafe.svg', mimeType: 'image/svg+xml', buffer: Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"/>') });
  await expect(section(page).getByRole('alert')).toBeVisible();
  await section(page).getByRole('button', { name: 'Lưu bản nháp', exact: true }).click();
  await expect.poll(() => api.saves.length).toBe(1);
  expect(api.saves[0].config.qrPayload).toBe(transferPayload());
});

test('preserves unsaved receiver, decoded QR and token across failed background refresh and recovery', async ({ page }) => {
  const api = await fixture(page);
  await page.goto('/admin/connection');
  await upload(page, transferPayload());
  await expect(field(page, 'accountNumber')).toHaveValue('0000000000');
  await field(page, 'storeName').fill('Unsubmitted receiver');
  await field(page, 'botToken').fill('unsaved-token-not-for-storage');
  api.failReads(true);
  await page.getByRole('button', { name: 'Làm mới', exact: true }).first().click();
  await expect(section(page).getByRole('alert')).toBeVisible();
  await expect(field(page, 'storeName')).toHaveValue('Unsubmitted receiver');
  await expect(field(page, 'botToken')).toHaveValue('unsaved-token-not-for-storage');
  await expect(field(page, 'accountNumber')).toHaveValue('0000000000');
  api.failReads(false);
  await section(page).getByRole('button', { name: 'Tải lại cấu hình SePay', exact: true }).click();
  await expect(section(page).getByRole('alert')).toHaveCount(0);
  await expect(field(page, 'storeName')).toHaveValue('Unsubmitted receiver');
  await section(page).getByRole('button', { name: 'Lưu bản nháp', exact: true }).click();
  await expect.poll(() => api.saves.length).toBe(1);
  expect(api.saves[0].config.qrPayload).toBe(transferPayload());
  expect(api.saves[0].botToken).toBe('unsaved-token-not-for-storage');
});

test('does not echo token in API errors and explicitly recovers revision conflicts', async ({ page }) => {
  const api = await fixture(page);
  await page.goto('/admin/connection');
  await field(page, 'botToken').fill('never-render-or-cache-me');
  api.fail(true);
  await section(page).getByRole('button', { name: 'Lưu bản nháp', exact: true }).click();
  await expect(section(page).getByRole('alert')).toBeVisible();
  expect(await page.locator('body').innerText()).not.toContain('never-render-or-cache-me');
  await expect(field(page, 'botToken')).toHaveValue('never-render-or-cache-me');
  api.fail(false); api.conflict(true);
  await section(page).getByRole('button', { name: 'Lưu bản nháp', exact: true }).click();
  await section(page).getByRole('button', { name: 'Tải lại cấu hình · bỏ bản nhập hiện tại' }).click();
  await expect(field(page, 'botToken')).toHaveValue('');
  api.conflict(false);
  await field(page, 'storeName').fill('New draft');
  await section(page).getByRole('button', { name: 'Lưu bản nháp', exact: true }).click();
  await expect.poll(() => api.config.revision).toBe(1);
  expect(api.config.config.mode).toBe('disabled');
});

test('failed configuration reads never render an editable blank replacement', async ({ page }) => {
  const api = await fixture(page);
  await page.route(/\/api\/v1\/sepay-store\/config$/, route => route.fulfill({ status: 503, json: { error: 'SEPAY_UNAVAILABLE' } }));
  await page.goto('/admin/connection');
  await expect(section(page).getByRole('alert')).toBeVisible();
  await expect(page.getByTestId('sepay-admin-form')).toHaveCount(0);
  await expect(page.getByTestId('sepay-qr-upload')).toHaveCount(0);
  expect(api.saves).toEqual([]);
});

test('receiver edits revoke attestation and prevent activating mismatched QR', async ({ page }) => {
  await fixture(page);
  await page.goto('/admin/connection');
  await upload(page, transferPayload());
  await expect(field(page, 'accountNumber')).toHaveValue('0000000000');
  await field(page, 'receiverVerified').check();
  await field(page, 'sourceSeparated').check();
  await field(page, 'accountNumber').fill('WRONGACCOUNT');
  await expect(field(page, 'receiverVerified')).not.toBeChecked();
  await expect(field(page, 'sourceSeparated')).not.toBeChecked();
  await field(page, 'receiverVerified').check();
  await expect(section(page).getByRole('alert')).toBeVisible();
  await expect(section(page).getByRole('button', { name: 'Lưu & bật SePay', exact: true })).toBeDisabled();
  await field(page, 'sourceSeparated').check();
  await upload(page, transferPayload());
  await expect(field(page, 'accountNumber')).toHaveValue('0000000000');
  await expect(field(page, 'receiverVerified')).not.toBeChecked();
  await expect(field(page, 'sourceSeparated')).not.toBeChecked();
});

for (const role of ['OPERATOR', 'VIEWER']) test(`${role} cannot fetch owner config or access secret/activation actions`, async ({ page }) => {
  const api = await fixture(page, role);
  await page.goto('/admin/connection');
  await expect(section(page)).toBeVisible();
  await expect(page.getByTestId('sepay-admin-form')).toHaveCount(0);
  await expect(field(page, 'botToken')).toHaveCount(0);
  await expect(page.getByTestId('sepay-qr-upload')).toHaveCount(0);
  await expect(section(page).getByRole('button', { name: 'Đăng ký webhook Telegram' })).toHaveCount(0);
  expect(api.reads).toBe(0);
  expect(api.saves).toEqual([]);
});
