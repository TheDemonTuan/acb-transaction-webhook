export interface SePayAdminFields {
  mode: 'disabled' | 'observe' | 'active';
  storeKey: string;
  storeName: string;
  bankCode: string;
  bankName: string;
  accountNumber: string;
  notificationAccountNumber: string;
  accountName: string;
  qrPayload: string;
  botId: string;
  chatId: string;
  senderBotId: string;
  topicId: string;
  activationAt: string;
  receiverVerified: boolean;
  sourceSeparated: boolean;
}

export interface SePayAdminConfig {
  revision: number;
  config: SePayAdminFields;
  hasBotToken: boolean;
  hasWebhookSecret: boolean;
  lastMessageAt: string | null;
  reviewCount: number;
}

export interface SePayAdminConfigInput {
  revision: number;
  config: SePayAdminFields;
  botToken: string;
}

export interface SePayTelegramStatus {
  registeredUrl: string;
  pendingUpdateCount: number;
  lastErrorAt: string | null;
  lastErrorMessage: string;
  botVerified: boolean;
}

export const SEPAY_NOTIFICATION_TEMPLATE = `SEPAY_STORE_V1
direction={{vao_hay_ra}}
amount={{amount}}
account={{account_number}}
bank={{bank_name}}
time={{transaction_date}}
reference={{reference_number}}
content={{transaction_content}}`;

export const sepayModeLabel = (mode?: string): string =>
  mode === 'active' ? 'Đã bật nhận thông báo' : mode === 'observe' ? 'Đang kiểm tra · chưa ghi tiền' : 'Chưa thiết lập / đang tắt';
