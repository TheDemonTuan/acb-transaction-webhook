// Public receiver snapshot; ACTIVE means ingest is enabled, not bank connectivity.
export interface SePayStoreConfig {
  provider: 'SEPAY';
  status: 'DISABLED' | 'OBSERVING' | 'ACTIVE';
  // Receiver fields are empty unless status is ACTIVE.
  storeName: string;
  bank: string;
  accountNumber: string;
  accountName: string;
  qrPayload: string;
  lastMessageAt: string | null;
}
