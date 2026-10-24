import { expect, test } from '@playwright/test';
import { installVoiceFixture, manifest } from './voice-audio-fixture';

test('real OfflineAudioContext gain evidence and pinned compressed-pack playback', async ({ page }, testInfo) => {
  test.setTimeout(180000);
  await installVoiceFixture(page);
  const logs: string[] = [];
  page.on('console', message => {
    if (message.text().startsWith('VIENEU_REAL_AUDIO_RMS')) logs.push(message.text());
  });
  await page.goto('/e2e/voice-audio-harness.html');
  await page.getByRole('button', { name: 'Run real browser audio', exact: true }).click();
  const result = page.locator('#result');
  await expect(result).toHaveAttribute('data-status', /completed|failed/, { timeout: 150000 });
  const evidence = await result.textContent();
  await testInfo.attach('vieneu-real-browser-audio.json', { body: evidence ?? '', contentType: 'application/json' });
  await testInfo.attach('vieneu-real-browser-rms.log', { body: logs.join('\n'), contentType: 'text/plain' });
  expect(await result.getAttribute('data-status'), evidence ?? 'Missing evidence').toBe('completed');
  const report = JSON.parse(evidence!);
  expect(report.offline.clipCount).toBe(manifest.clips.length);
  expect(report.offline.samples.map((clip: { clipId: string }) => clip.clipId).sort()).toEqual([
    'amount-1000', 'amount-1000000', 'amount-500000', 'classic-001-500000',
  ]);
  expect(report.compressed.sample.audioPath).toBe('compressed-pack');
  expect(report.compressed.stats.decodedBytes).toBe(16 * 1024 * 1024);
  expect(logs).toHaveLength(1);
});
