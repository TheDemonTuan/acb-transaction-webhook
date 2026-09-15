import React, { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  Bell,
  Plus,
  Power,
  RefreshCw,
  Key,
  CheckCircle2,
  AlertCircle,
  Smartphone,
  Globe,
  Send,
  Sliders,
  RotateCcw,
  Copy,
  Info,
  ExternalLink,
} from 'lucide-react';
import {
  createNotificationChannel,
  fetchNotificationChannels,
  fetchNotificationProviders,
  rotateChannelSecret,
  testNotificationChannel,
  toggleNotificationChannel,
  updateNotificationChannel,
} from '../../shared/api/queries';
import { queryKeys } from '../../shared/api/query-keys';
import { formatErrorMessage } from '../../content/error-copy';
import type { BarkConfig, NotificationChannel } from '../../realtime-types';

const DEFAULT_BARK_ICON = 'https://api.vietqr.io/img/ACB.png';
const BLOCKED_URL_SCHEME = /^\s*(?:javascript|data|vbscript)\s*:/i;

const sanitizeBarkIcon = (value: string | null | undefined): string => {
  const candidate = (value || '').trim();
  if (!candidate || BLOCKED_URL_SCHEME.test(candidate)) {
    return DEFAULT_BARK_ICON;
  }
  return encodeURI(candidate);
};

export const NotificationChannelsPage: React.FC = () => {
  const queryClient = useQueryClient();

  // Tab: 'WEBHOOK' | 'BARK'
  const [selectedProvider, setSelectedProvider] = useState<'WEBHOOK' | 'BARK'>('BARK');

  // Form states
  const [name, setName] = useState('');
  const [url, setUrl] = useState('');
  const [deviceKey, setDeviceKey] = useState('');
  const [showDeviceKey, setShowDeviceKey] = useState(false);

  // Bark config form
  const [barkGroup, setBarkGroup] = useState('ACB');
  const [barkLevel, setBarkLevel] = useState<'passive' | 'active' | 'timeSensitive'>('timeSensitive');
  const [barkSound, setBarkSound] = useState('shake');
  const [barkIcon, setBarkIcon] = useState(DEFAULT_BARK_ICON);
  const [failedBarkIcon, setFailedBarkIcon] = useState<string | null>(null);
  const [includeBalance, setIncludeBalance] = useState(false);
  const [includeDescription, setIncludeDescription] = useState(true);
  const [dashboardLink, setDashboardLink] = useState(true);
  const [showAdvancedBark, setShowAdvancedBark] = useState(false);

  // Status & action notifications
  const [notice, setNotice] = useState<string | null>(null);
  const [errorNotice, setErrorNotice] = useState<string | null>(null);
  const [isCreating, setIsCreating] = useState(false);
  const [testingId, setTestingId] = useState<string | null>(null);
  const [togglingId, setTogglingId] = useState<string | null>(null);

  // Dialogs
  const [createdSecretDialog, setCreatedSecretDialog] = useState<{
    name: string;
    secret: string;
  } | null>(null);
  const [copiedSecret, setCopiedSecret] = useState(false);

  // Edit channel dialog
  const [editingChannel, setEditingChannel] = useState<NotificationChannel | null>(null);
  const [editName, setEditName] = useState('');
  const [editUrl, setEditUrl] = useState('');
  const [editBarkConfig, setEditBarkConfig] = useState<BarkConfig>({
    group: 'ACB',
    level: 'timeSensitive',
    sound: 'shake',
    icon: DEFAULT_BARK_ICON,
    includeBalance: false,
    includeDescription: true,
    dashboardLink: true,
  });
  const [isUpdating, setIsUpdating] = useState(false);

  // Rotate secret dialog
  const [rotatingChannel, setRotatingChannel] = useState<NotificationChannel | null>(null);
  const [newDeviceKeyInput, setNewDeviceKeyInput] = useState('');
  const [isRotating, setIsRotating] = useState(false);

  // Queries
  const { data: channelsData, isLoading: loadingChannels, refetch: refetchChannels } = useQuery({
    queryKey: queryKeys.notificationChannels,
    queryFn: fetchNotificationChannels,
  });

  const {
    data: providersData,
    isLoading: loadingProviders,
    isError: errorProviders,
    isFetching: isFetchingProviders,
    refetch: refetchProviders,
  } = useQuery({
    queryKey: queryKeys.notificationProviders,
    queryFn: fetchNotificationProviders,
  });

  const channels = channelsData?.items || [];
  const providers = providersData?.providers || [];
  const barkProvider = providers.find((p) => p.id === 'BARK');

  const barkChannels = channels.filter((c) => c.provider === 'BARK');
  const activeBarkCount = barkChannels.filter((c) => c.status === 'ACTIVE').length;
  const webhookChannels = channels.filter((c) => c.provider === 'WEBHOOK');
  const activeWebhookCount = webhookChannels.filter((c) => c.status === 'ACTIVE').length;

  const getBarkStatus = () => {
    if (loadingProviders) {
      return {
        label: 'Đang tải...',
        className: 'bg-stone-100 text-stone-500 border-stone-200',
      };
    }
    if (errorProviders || !barkProvider || barkProvider.status === 'unknown' || barkProvider.status === 'error') {
      return {
        label: 'Không xác định được trạng thái',
        className: 'bg-stone-100 text-stone-600 border-stone-300',
      };
    }
    if (barkProvider.configured) {
      return {
        label: 'Đã cấu hình',
        className: 'bg-emerald-50 text-emerald-700 border-emerald-200',
      };
    }
    return {
      label: 'Chưa cấu hình',
      className: 'bg-amber-50 text-amber-700 border-amber-200',
    };
  };

  const barkStatus = getBarkStatus();
  const isRefreshing = loadingChannels || isFetchingProviders;

  const handleRefresh = () => {
    refetchChannels();
    refetchProviders();
  };

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim()) return;

    if (selectedProvider === 'WEBHOOK' && !url.trim()) return;
    if (selectedProvider === 'BARK' && !deviceKey.trim()) return;

    setIsCreating(true);
    setNotice(null);
    setErrorNotice(null);

    try {
      if (selectedProvider === 'WEBHOOK') {
        const created = await createNotificationChannel({
          provider: 'WEBHOOK',
          name: name.trim(),
          url: url.trim(),
        });
        setNotice('Đã tạo kênh Webhook ở trạng thái DISABLED.');
        if (created.secret) {
          setCreatedSecretDialog({ name: created.name, secret: created.secret });
        }
      } else {
        await createNotificationChannel({
          provider: 'BARK',
          name: name.trim(),
          deviceKey: deviceKey.trim(),
          barkConfig: {
            group: barkGroup.trim() || 'ACB',
            level: barkLevel,
            sound: barkSound.trim() || 'shake',
            icon: sanitizeBarkIcon(barkIcon),
            includeBalance,
            includeDescription,
            dashboardLink,
          },
        });
        setNotice('Đã tạo kênh Bark iPhone ở trạng thái DISABLED. Hãy bấm "Gửi thử" trước khi bật.');
      }

      setName('');
      setUrl('');
      setDeviceKey('');
      queryClient.invalidateQueries({ queryKey: queryKeys.notificationChannels });
    } catch (err) {
      setErrorNotice(formatErrorMessage(err));
    } finally {
      setIsCreating(false);
    }
  };

  const handleToggle = async (channel: NotificationChannel) => {
    setTogglingId(channel.id);
    setNotice(null);
    setErrorNotice(null);

    const action = channel.status === 'ACTIVE' ? 'disable' : 'enable';
    try {
      await toggleNotificationChannel(channel.id, action);
      queryClient.invalidateQueries({ queryKey: queryKeys.notificationChannels });
      setNotice(`Đã chuyển kênh "${channel.name}" sang trạng thái ${action === 'enable' ? 'ACTIVE' : 'DISABLED'}.`);
    } catch (err) {
      setErrorNotice(formatErrorMessage(err));
    } finally {
      setTogglingId(null);
    }
  };

  const handleTest = async (channel: NotificationChannel) => {
    setTestingId(channel.id);
    setNotice(null);
    setErrorNotice(null);

    try {
      const res = await testNotificationChannel(channel.id);
      setNotice(`✅ ${res.message || 'Đã gửi thông báo thử thành công!'}${res.latencyMs ? ` (${res.latencyMs}ms)` : ''}`);
    } catch (err) {
      setErrorNotice(formatErrorMessage(err));
    } finally {
      setTestingId(null);
    }
  };

  const openEditModal = (ch: NotificationChannel) => {
    setEditingChannel(ch);
    setEditName(ch.name);
    setEditUrl(ch.url || '');
    if (ch.barkConfig) {
      setEditBarkConfig({ ...ch.barkConfig });
    } else {
      setEditBarkConfig({
        group: 'ACB',
        level: 'timeSensitive',
        sound: 'shake',
        icon: DEFAULT_BARK_ICON,
        includeBalance: false,
        includeDescription: true,
        dashboardLink: true,
      });
    }
  };

  const handleSaveEdit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!editingChannel || !editName.trim()) return;

    setIsUpdating(true);
    setNotice(null);
    setErrorNotice(null);

    try {
      await updateNotificationChannel(editingChannel.id, {
        expectedRevision: editingChannel.revision,
        name: editName.trim(),
        url: editingChannel.provider === 'WEBHOOK' ? editUrl.trim() : undefined,
        barkConfig: editingChannel.provider === 'BARK'
          ? { ...editBarkConfig, icon: sanitizeBarkIcon(editBarkConfig.icon) }
          : undefined,
      });
      setNotice(`Đã cập nhật kênh "${editName.trim()}".`);
      setEditingChannel(null);
      queryClient.invalidateQueries({ queryKey: queryKeys.notificationChannels });
    } catch (err) {
      setErrorNotice(formatErrorMessage(err));
    } finally {
      setIsUpdating(false);
    }
  };

  const openRotateModal = (ch: NotificationChannel) => {
    setRotatingChannel(ch);
    setNewDeviceKeyInput('');
  };

  const handleConfirmRotate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!rotatingChannel) return;

    setIsRotating(true);
    setNotice(null);
    setErrorNotice(null);

    try {
      const res = await rotateChannelSecret(
        rotatingChannel.id,
        rotatingChannel.provider === 'BARK' ? newDeviceKeyInput.trim() : undefined
      );

      if (rotatingChannel.provider === 'WEBHOOK' && res.secret) {
        setCreatedSecretDialog({ name: rotatingChannel.name, secret: res.secret });
        setNotice('Đã tạo khóa HMAC mới. Vui lòng lưu lại khóa.');
      } else {
        setNotice(`Đã cập nhật khóa cho kênh "${rotatingChannel.name}".`);
      }
      setRotatingChannel(null);
      queryClient.invalidateQueries({ queryKey: queryKeys.notificationChannels });
    } catch (err) {
      setErrorNotice(formatErrorMessage(err));
    } finally {
      setIsRotating(false);
    }
  };

  const copyCreatedSecret = () => {
    if (!createdSecretDialog) return;
    navigator.clipboard.writeText(createdSecretDialog.secret);
    setCopiedSecret(true);
    setTimeout(() => setCopiedSecret(false), 2000);
  };

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h2 className="text-2xl font-bold tracking-tight text-stone-900">Kênh thông báo</h2>
          <p className="text-sm text-stone-500 mt-0.5">
            Quản lý các đích nhận thông báo giao dịch ACB (Bark cho iPhone & Webhook tùy chỉnh)
          </p>
        </div>
        <button
          type="button"
          onClick={handleRefresh}
          className="inline-flex items-center self-start sm:self-auto gap-2 px-3 py-2 rounded-xl text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer"
        >
          <RefreshCw className={`w-3.5 h-3.5 ${isRefreshing ? 'animate-spin' : ''}`} />
          <span>Làm mới</span>
        </button>
      </div>

      {/* Provider Status Banners */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {/* Bark Card */}
        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs space-y-3">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2.5">
              <div className="w-9 h-9 rounded-xl bg-purple-50 flex items-center justify-center text-purple-600">
                <Smartphone className="w-5 h-5" />
              </div>
              <div>
                <h4 className="text-sm font-bold text-stone-900">Bark • iPhone Push</h4>
                <p className="text-xs text-stone-500">Đẩy trực tiếp tới iPhone qua Bark self-host</p>
              </div>
            </div>
            <span
              className={`text-2xs font-bold px-2 py-0.5 rounded-full border ${barkStatus.className}`}
            >
              {barkStatus.label}
            </span>
          </div>
          <div className="text-xs text-stone-600">
            {barkChannels.length} thiết bị đã lưu ({activeBarkCount} đang bật)
          </div>
          {barkProvider?.publicUrl && (
            <div className="text-2xs text-stone-600 bg-stone-50 p-2.5 rounded-xl border border-stone-200/80 flex items-center justify-between">
              <span className="font-mono truncate">Server URL: {barkProvider.publicUrl}</span>
              <button
                type="button"
                onClick={() => navigator.clipboard.writeText(barkProvider.publicUrl || '')}
                className="text-stone-500 hover:text-stone-900 shrink-0 ml-2"
                title="Sao chép URL"
              >
                <Copy className="w-3.5 h-3.5" />
              </button>
            </div>
          )}
        </div>

        {/* Webhook Card */}
        <div className="bg-white p-5 rounded-2xl border border-stone-200 shadow-2xs space-y-3">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2.5">
              <div className="w-9 h-9 rounded-xl bg-blue-50 flex items-center justify-center text-blue-600">
                <Globe className="w-5 h-5" />
              </div>
              <div>
                <h4 className="text-sm font-bold text-stone-900">Webhook HMAC</h4>
                <p className="text-xs text-stone-500">Gửi HTTP POST JSON có chữ ký SHA-256</p>
              </div>
            </div>
            <span className="text-2xs font-bold px-2 py-0.5 rounded-full bg-emerald-50 text-emerald-700 border border-emerald-200">
              Sẵn sàng
            </span>
          </div>
          <div className="text-xs text-stone-600">
            {webhookChannels.length} webhook đã lưu ({activeWebhookCount} đang bật)
          </div>
          <p className="text-2xs text-stone-500">
            Dùng chung cơ chế bảo vệ SSRF, retry và dead-letter với độ bền dữ liệu cao.
          </p>
        </div>
      </div>

      {/* Notifications / Alerts */}
      {notice && (
        <div className="p-4 rounded-xl bg-emerald-50 border border-emerald-200 text-sm text-emerald-900 flex items-center gap-2 animate-in fade-in">
          <CheckCircle2 className="w-5 h-5 text-emerald-600 shrink-0" />
          <span>{notice}</span>
        </div>
      )}

      {errorNotice && (
        <div className="p-4 rounded-xl bg-rose-50 border border-rose-200 text-sm text-rose-900 flex items-center gap-2 animate-in fade-in">
          <AlertCircle className="w-5 h-5 text-rose-600 shrink-0" />
          <span>{errorNotice}</span>
        </div>
      )}

      {/* Webhook Secret Created Modal */}
      {createdSecretDialog && (
        <div className="p-5 rounded-2xl bg-amber-50 border border-amber-200 shadow-sm space-y-3">
          <div className="flex items-start justify-between gap-4">
            <div className="space-y-1">
              <div className="flex items-center gap-2">
                <Key className="w-5 h-5 text-amber-600" />
                <h4 className="font-bold text-base text-amber-900">
                  Lưu lại khóa bí mật Webhook (Secret)
                </h4>
              </div>
              <p className="text-xs text-amber-800 leading-relaxed">
                Khóa này dùng để kiểm tra chữ ký xác thực HMAC-SHA256 trên webhook receiver của bạn. Vì lý do an toàn,{' '}
                <strong>khóa bí mật chỉ hiển thị duy nhất một lần tại đây</strong>.
              </p>
            </div>
            <button
              type="button"
              onClick={() => setCreatedSecretDialog(null)}
              className="text-xs font-semibold px-3 py-1.5 rounded-lg bg-amber-200/70 hover:bg-amber-300 text-amber-900 transition cursor-pointer shrink-0"
            >
              Tôi đã lưu khóa
            </button>
          </div>

          <div className="flex items-center gap-2 bg-white p-3 rounded-xl border border-amber-200 font-mono text-sm break-all">
            <span className="flex-1 text-stone-900 font-semibold select-all">
              {createdSecretDialog.secret}
            </span>
            <button
              type="button"
              onClick={copyCreatedSecret}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition cursor-pointer shrink-0"
            >
              {copiedSecret ? 'Đã sao chép!' : 'Sao chép khóa'}
            </button>
          </div>
        </div>
      )}

      {/* Create New Channel Form */}
      <div className="bg-white rounded-2xl border border-stone-200 p-6 shadow-2xs space-y-5">
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3 border-b border-stone-100 pb-4">
          <div>
            <h3 className="text-base font-bold text-stone-900">Thêm kênh thông báo mới</h3>
            <p className="text-xs text-stone-500">Mỗi kênh mới sẽ khởi tạo ở trạng thái TẠM TẮT (DISABLED).</p>
          </div>

          {/* Provider selector */}
          <div className="inline-flex p-1 bg-stone-100 rounded-xl border border-stone-200">
            <button
              type="button"
              onClick={() => setSelectedProvider('BARK')}
              className={`inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold transition cursor-pointer ${
                selectedProvider === 'BARK'
                  ? 'bg-white text-purple-700 shadow-2xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              <Smartphone className="w-3.5 h-3.5" />
              <span>Bark (iPhone)</span>
            </button>
            <button
              type="button"
              onClick={() => setSelectedProvider('WEBHOOK')}
              className={`inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold transition cursor-pointer ${
                selectedProvider === 'WEBHOOK'
                  ? 'bg-white text-blue-700 shadow-2xs'
                  : 'text-stone-600 hover:text-stone-900'
              }`}
            >
              <Globe className="w-3.5 h-3.5" />
              <span>Webhook</span>
            </button>
          </div>
        </div>

        {/* Onboarding helper for Bark */}
        {selectedProvider === 'BARK' && (
          <div className="bg-purple-50/70 border border-purple-200/80 rounded-xl p-4 text-xs text-purple-900 space-y-2">
            <div className="flex items-center gap-1.5 font-bold text-purple-950">
              <Info className="w-4 h-4 text-purple-600 shrink-0" />
              <span>Hướng dẫn kết nối Bark trên iPhone:</span>
            </div>
            <ol className="list-decimal list-inside space-y-1 pl-1 text-purple-900/90 leading-relaxed">
              <li>Cài đặt ứng dụng <strong>Bark</strong> trên App Store iOS.</li>
              <li>Thêm server self-host: <strong>{barkProvider?.publicUrl || 'https://bark.tuannguyenviet.site'}</strong></li>
              <li>Sao chép <strong>Device Key</strong> hiển thị trong ứng dụng Bark.</li>
              <li>Dán Device Key vào form bên dưới để tạo kênh.</li>
              <li>Bấm <strong>Gửi thử</strong> để nhận chuông test trên điện thoại, sau đó bật kích hoạt kênh.</li>
            </ol>
          </div>
        )}

        <form onSubmit={handleCreate} className="space-y-4">
          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <label htmlFor="channel-name" className="block text-xs font-semibold text-stone-700 mb-1">
                {selectedProvider === 'BARK' ? 'Tên thiết bị / iPhone' : 'Tên kênh Webhook'}
              </label>
              <input
                id="channel-name"
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={selectedProvider === 'BARK' ? 'Ví dụ: iPhone 16 Pro (Tuấn)' : 'Ví dụ: Máy chủ Kế toán'}
                required
                className="w-full px-3.5 py-2.5 rounded-xl border border-stone-200 text-sm bg-stone-50/30 focus:bg-white focus:outline-none focus:ring-2 focus:ring-stone-900/10 focus:border-stone-900 transition"
              />
            </div>

            {selectedProvider === 'WEBHOOK' ? (
              <div>
                <label htmlFor="channel-url" className="block text-xs font-semibold text-stone-700 mb-1">
                  URL Webhook (HTTPS)
                </label>
                <input
                  id="channel-url"
                  type="url"
                  value={url}
                  onChange={(e) => setUrl(e.target.value)}
                  placeholder="https://api.yourdomain.com/webhooks/acb"
                  required
                  className="w-full px-3.5 py-2.5 rounded-xl border border-stone-200 text-sm bg-stone-50/30 focus:bg-white focus:outline-none focus:ring-2 focus:ring-stone-900/10 focus:border-stone-900 transition font-mono"
                />
              </div>
            ) : (
              <div>
                <label htmlFor="bark-device-key" className="block text-xs font-semibold text-stone-700 mb-1 flex items-center justify-between">
                  <span>Bark Device Key</span>
                  <button
                    type="button"
                    onClick={() => setShowDeviceKey(!showDeviceKey)}
                    className="text-2xs text-stone-500 hover:text-stone-800"
                  >
                    {showDeviceKey ? 'Ẩn key' : 'Hiện key'}
                  </button>
                </label>
                <input
                  id="bark-device-key"
                  type={showDeviceKey ? 'text' : 'password'}
                  value={deviceKey}
                  onChange={(e) => setDeviceKey(e.target.value)}
                  placeholder="Dán Device Key từ ứng dụng Bark"
                  required
                  className="w-full px-3.5 py-2.5 rounded-xl border border-stone-200 text-sm bg-stone-50/30 focus:bg-white focus:outline-none focus:ring-2 focus:ring-stone-900/10 focus:border-stone-900 transition font-mono"
                />
              </div>
            )}
          </div>

          {/* Advanced Bark Options Toggle */}
          {selectedProvider === 'BARK' && (
            <div className="space-y-3 pt-2">
              <button
                type="button"
                onClick={() => setShowAdvancedBark(!showAdvancedBark)}
                className="inline-flex items-center gap-1.5 text-xs font-semibold text-stone-600 hover:text-stone-900 transition cursor-pointer"
              >
                <Sliders className="w-3.5 h-3.5" />
                <span>{showAdvancedBark ? 'Ẩn tùy chọn nâng cao' : 'Tùy chỉnh thông báo (Âm thanh, Mức độ ưu tiên, Số dư)'}</span>
              </button>

              {showAdvancedBark && (
                <div className="p-4 rounded-xl bg-stone-50 border border-stone-200 grid grid-cols-1 sm:grid-cols-3 gap-4 animate-in fade-in">
                  <div>
                    <label htmlFor="bark-group" className="block text-2xs font-semibold text-stone-700 mb-1">
                      Nhóm (Group)
                    </label>
                    <input
                      id="bark-group"
                      type="text"
                      value={barkGroup}
                      onChange={(e) => setBarkGroup(e.target.value)}
                      placeholder="ACB"
                      className="w-full px-3 py-2 rounded-lg border border-stone-200 text-xs bg-white focus:outline-none"
                    />
                  </div>

                  <div>
                    <label htmlFor="bark-sound" className="block text-2xs font-semibold text-stone-700 mb-1">
                      Âm thanh chuông
                    </label>
                    <select
                      id="bark-sound"
                      value={barkSound}
                      onChange={(e) => setBarkSound(e.target.value)}
                      className="w-full px-3 py-2 rounded-lg border border-stone-200 text-xs bg-white focus:outline-none"
                    >
                      <option value="shake">shake (Mặc định)</option>
                      <option value="bell">bell</option>
                      <option value="chime">chime</option>
                      <option value="minuet">minuet</option>
                      <option value="glass">glass</option>
                      <option value="birdsong">birdsong</option>
                      <option value="electronic">electronic</option>
                    </select>
                  </div>

                  <div>
                    <label htmlFor="bark-level" className="block text-2xs font-semibold text-stone-700 mb-1">
                      Mức độ ưu tiên
                    </label>
                    <select
                      id="bark-level"
                      value={barkLevel}
                      onChange={(e) => setBarkLevel(e.target.value as any)}
                      className="w-full px-3 py-2 rounded-lg border border-stone-200 text-xs bg-white focus:outline-none"
                    >
                      <option value="timeSensitive">timeSensitive (Quan trọng - Sáng màn hình)</option>
                      <option value="active">active (Bình thường)</option>
                      <option value="passive">passive (Yên lặng)</option>
                    </select>
                  </div>

                  <div className="sm:col-span-3">
                    <label htmlFor="bark-icon" className="block text-2xs font-semibold text-stone-700 mb-1">
                      Icon thông báo (URL ảnh hiển thị trên iPhone)
                    </label>
                    <div className="flex items-center gap-2.5">
                      {failedBarkIcon === sanitizeBarkIcon(barkIcon) ? (
                        <div
                          role="img"
                          aria-label="Không tải được icon preview"
                          className="w-8 h-8 rounded-lg border border-stone-200 bg-stone-100 shrink-0 flex items-center justify-center text-stone-400"
                        >
                          <Smartphone className="w-4 h-4" />
                        </div>
                      ) : (
                        <img
                          src={sanitizeBarkIcon(barkIcon)}
                          alt="Icon preview"
                          className="w-8 h-8 rounded-lg border border-stone-200 object-contain bg-white shrink-0 p-0.5 shadow-2xs"
                          onError={() => setFailedBarkIcon(sanitizeBarkIcon(barkIcon))}
                        />
                      )}
                      <input
                        id="bark-icon"
                        type="url"
                        value={barkIcon}
                        onChange={(e) => setBarkIcon(e.target.value)}
                        placeholder={DEFAULT_BARK_ICON}
                        className="w-full px-3 py-2 rounded-lg border border-stone-200 text-xs bg-white focus:outline-none font-mono"
                      />
                    </div>
                  </div>

                  <div className="sm:col-span-3 flex flex-wrap gap-6 pt-2 border-t border-stone-200/60">
                    <label className="flex items-center gap-2 text-xs text-stone-700 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={includeDescription}
                        onChange={(e) => setIncludeDescription(e.target.checked)}
                        className="rounded border-stone-300 text-stone-900 focus:ring-stone-900"
                      />
                      <span>Hiển thị nội dung chuyển khoản</span>
                    </label>

                    <label className="flex items-center gap-2 text-xs text-stone-700 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={includeBalance}
                        onChange={(e) => setIncludeBalance(e.target.checked)}
                        className="rounded border-stone-300 text-stone-900 focus:ring-stone-900"
                      />
                      <span>Hiển thị số dư tài khoản sau giao dịch</span>
                    </label>

                    <label className="flex items-center gap-2 text-xs text-stone-700 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={dashboardLink}
                        onChange={(e) => setDashboardLink(e.target.checked)}
                        className="rounded border-stone-300 text-stone-900 focus:ring-stone-900"
                      />
                      <span>Mở chi tiết giao dịch khi chạm thông báo</span>
                    </label>
                  </div>
                </div>
              )}
            </div>
          )}

          <div className="flex justify-end pt-2">
            <button
              type="submit"
              disabled={isCreating}
              className="inline-flex items-center gap-2 px-5 py-2.5 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 disabled:opacity-50 transition shadow-xs cursor-pointer"
            >
              <Plus className="w-3.5 h-3.5" />
              <span>{isCreating ? 'Đang tạo kênh...' : selectedProvider === 'BARK' ? 'Tạo kênh Bark (iPhone)' : 'Tạo kênh Webhook'}</span>
            </button>
          </div>
        </form>
      </div>

      {/* Channels List Table */}
      <div className="bg-white rounded-2xl border border-stone-200 shadow-2xs overflow-hidden">
        <div className="px-6 py-4 border-b border-stone-100 flex items-center justify-between">
          <div className="flex items-center gap-2">
            <Bell className="w-4 h-4 text-stone-500" />
            <h3 className="font-bold text-stone-900 text-base">Danh sách kênh thông báo</h3>
          </div>
          <span className="text-xs text-stone-500">{channels.length} kênh đã tạo</span>
        </div>

        {channels.length === 0 ? (
          <div className="p-12 text-center text-xs text-stone-500">
            {loadingChannels ? 'Đang tải danh sách kênh...' : 'Chưa có kênh thông báo nào. Hãy thêm kênh đầu tiên ở trên.'}
          </div>
        ) : (
          <div className="divide-y divide-stone-100">
            {channels.map((ch) => {
              const isBark = ch.provider === 'BARK';
              const isActive = ch.status === 'ACTIVE';

              return (
                <div
                  key={ch.id}
                  data-testid="channel-row"
                  className="p-5 sm:px-6 flex flex-col sm:flex-row sm:items-center justify-between gap-4 hover:bg-stone-50/50 transition"
                >
                  <div className="space-y-1.5 max-w-xl">
                    <div className="flex flex-wrap items-center gap-2">
                      {isBark && (
                        <img
                          src={sanitizeBarkIcon(ch.barkConfig?.icon)}
                          alt="Icon"
                          className="w-5 h-5 rounded-md border border-stone-200 object-contain bg-white shrink-0"
                          onError={(e) => { (e.currentTarget as HTMLImageElement).src = DEFAULT_BARK_ICON; }}
                        />
                      )}
                      <span className="font-bold text-sm text-stone-900">{ch.name}</span>

                      {/* Provider Badge */}
                      <span
                        className={`text-2xs font-semibold px-2 py-0.5 rounded-md border flex items-center gap-1 ${
                          isBark
                            ? 'bg-purple-50 text-purple-700 border-purple-200'
                            : 'bg-blue-50 text-blue-700 border-blue-200'
                        }`}
                      >
                        {isBark ? <Smartphone className="w-3 h-3" /> : <Globe className="w-3 h-3" />}
                        <span>{isBark ? 'Bark • iPhone' : 'Webhook'}</span>
                      </span>

                      {/* Status Badge */}
                      <span
                        className={`text-2xs font-semibold px-2 py-0.5 rounded-md border ${
                          isActive
                            ? 'bg-emerald-50 text-emerald-700 border-emerald-200'
                            : 'bg-stone-100 text-stone-600 border-stone-200'
                        }`}
                      >
                        {isActive ? 'Hoạt động' : 'Tạm tắt'}
                      </span>

                      <span className="text-2xs font-mono text-stone-500 bg-stone-100 px-1.5 py-0.5 rounded">
                        rev.{ch.revision}
                      </span>
                    </div>

                    {/* Details: URL or Bark settings */}
                    <div className="text-xs text-stone-500 flex flex-wrap items-center gap-x-4 gap-y-1">
                      {isBark ? (
                        <>
                          <span className="font-mono text-stone-600">Key: ●●●●●●●●</span>
                          {ch.barkConfig?.group && <span>Nhóm: {ch.barkConfig.group}</span>}
                          {ch.barkConfig?.sound && <span>Chuông: {ch.barkConfig.sound}</span>}
                          {ch.barkConfig?.level && <span>Ưu tiên: {ch.barkConfig.level}</span>}
                        </>
                      ) : (
                        <span className="font-mono text-stone-600 break-all">{ch.url}</span>
                      )}
                    </div>
                  </div>

                  {/* Actions */}
                  <div className="flex flex-wrap items-center gap-2 shrink-0">
                    {/* Test Button */}
                    <button
                      type="button"
                      onClick={() => handleTest(ch)}
                      disabled={testingId === ch.id}
                      className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer disabled:opacity-50"
                      title="Gửi thông báo thử nghiệm"
                    >
                      <Send className={`w-3 h-3 ${testingId === ch.id ? 'animate-spin' : ''}`} />
                      <span>{testingId === ch.id ? 'Đang gửi...' : 'Gửi thử'}</span>
                    </button>

                    {/* Edit Button */}
                    <button
                      type="button"
                      onClick={() => openEditModal(ch)}
                      className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer"
                    >
                      <Sliders className="w-3 h-3 text-stone-500" />
                      <span>Sửa</span>
                    </button>

                    {/* Rotate Secret Button */}
                    <button
                      type="button"
                      onClick={() => openRotateModal(ch)}
                      className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold bg-white border border-stone-200 text-stone-700 hover:bg-stone-50 transition shadow-2xs cursor-pointer"
                      title={isBark ? 'Cập nhật Device Key mới' : 'Đổi khóa bí mật HMAC mới'}
                    >
                      <Key className="w-3 h-3 text-stone-500" />
                      <span>{isBark ? 'Đổi Key' : 'Đổi Secret'}</span>
                    </button>

                    {/* Enable / Disable Toggle */}
                    <button
                      type="button"
                      onClick={() => handleToggle(ch)}
                      disabled={togglingId === ch.id}
                      className={`inline-flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-semibold transition shadow-2xs cursor-pointer disabled:opacity-50 ${
                        isActive
                          ? 'bg-rose-50 text-rose-700 border border-rose-200 hover:bg-rose-100'
                          : 'bg-emerald-600 text-white hover:bg-emerald-500'
                      }`}
                    >
                      <Power className="w-3 h-3" />
                      <span>{isActive ? 'Tắt kênh' : 'Kích hoạt'}</span>
                    </button>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* Edit Modal */}
      {editingChannel && (
        <div className="fixed inset-0 z-50 bg-stone-900/40 backdrop-blur-xs flex items-center justify-center p-4">
          <div className="bg-white rounded-2xl border border-stone-200 shadow-xl max-w-lg w-full p-6 space-y-4 animate-in zoom-in-95">
            <div className="flex items-center justify-between border-b border-stone-100 pb-3">
              <h3 className="text-base font-bold text-stone-900">
                Chỉnh sửa kênh: {editingChannel.name}
              </h3>
              <button
                type="button"
                onClick={() => setEditingChannel(null)}
                className="text-stone-400 hover:text-stone-600"
              >
                ✕
              </button>
            </div>

            <form onSubmit={handleSaveEdit} className="space-y-4">
              <div>
                <label htmlFor="edit-channel-name" className="block text-xs font-semibold text-stone-700 mb-1">Tên kênh</label>
                <input
                  id="edit-channel-name"
                  type="text"
                  value={editName}
                  onChange={(e) => setEditName(e.target.value)}
                  required
                  className="w-full px-3 py-2 rounded-xl border border-stone-200 text-sm focus:outline-none"
                />
              </div>

              {editingChannel.provider === 'WEBHOOK' ? (
                <div>
                  <label htmlFor="edit-channel-url" className="block text-xs font-semibold text-stone-700 mb-1">URL Webhook</label>
                  <input
                    id="edit-channel-url"
                    type="url"
                    value={editUrl}
                    onChange={(e) => setEditUrl(e.target.value)}
                    required
                    className="w-full px-3 py-2 rounded-xl border border-stone-200 text-sm font-mono focus:outline-none"
                  />
                </div>
              ) : (
                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <label className="block text-2xs font-semibold text-stone-700 mb-1">Nhóm</label>
                    <input
                      type="text"
                      value={editBarkConfig.group || ''}
                      onChange={(e) => setEditBarkConfig({ ...editBarkConfig, group: e.target.value })}
                      className="w-full px-3 py-1.5 rounded-lg border border-stone-200 text-xs focus:outline-none"
                    />
                  </div>

                  <div>
                    <label className="block text-2xs font-semibold text-stone-700 mb-1">Chuông</label>
                    <select
                      value={editBarkConfig.sound || 'shake'}
                      onChange={(e) => setEditBarkConfig({ ...editBarkConfig, sound: e.target.value })}
                      className="w-full px-3 py-1.5 rounded-lg border border-stone-200 text-xs focus:outline-none bg-white"
                    >
                      <option value="shake">shake</option>
                      <option value="bell">bell</option>
                      <option value="minuet">minuet</option>
                      <option value="chime">chime</option>
                      <option value="glass">glass</option>
                    </select>
                  </div>

                  <div className="col-span-2">
                    <label className="block text-2xs font-semibold text-stone-700 mb-1">Ưu tiên</label>
                    <select
                      value={editBarkConfig.level || 'timeSensitive'}
                      onChange={(e) => setEditBarkConfig({ ...editBarkConfig, level: e.target.value as any })}
                      className="w-full px-3 py-1.5 rounded-lg border border-stone-200 text-xs focus:outline-none bg-white"
                    >
                      <option value="timeSensitive">timeSensitive (Quan trọng)</option>
                      <option value="active">active (Bình thường)</option>
                      <option value="passive">passive (Yên lặng)</option>
                    </select>
                  </div>

                  <div className="col-span-2">
                    <label className="block text-2xs font-semibold text-stone-700 mb-1">Icon thông báo (URL ảnh)</label>
                    <div className="flex items-center gap-2">
                      <img
                        src={sanitizeBarkIcon(editBarkConfig.icon)}
                        alt="Icon"
                        className="w-7 h-7 rounded-md border border-stone-200 object-contain bg-white shrink-0 p-0.5"
                        onError={(e) => { (e.currentTarget as HTMLImageElement).src = DEFAULT_BARK_ICON; }}
                      />
                      <input
                        type="url"
                        value={editBarkConfig.icon || ''}
                        onChange={(e) => setEditBarkConfig({ ...editBarkConfig, icon: e.target.value })}
                        placeholder={DEFAULT_BARK_ICON}
                        className="w-full px-3 py-1.5 rounded-lg border border-stone-200 text-xs font-mono focus:outline-none"
                      />
                    </div>
                  </div>

                  <div className="col-span-2 space-y-2 pt-1 border-t border-stone-100">
                    <label className="flex items-center gap-2 text-xs text-stone-700 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={editBarkConfig.includeDescription}
                        onChange={(e) => setEditBarkConfig({ ...editBarkConfig, includeDescription: e.target.checked })}
                      />
                      <span>Hiển thị nội dung chuyển khoản</span>
                    </label>

                    <label className="flex items-center gap-2 text-xs text-stone-700 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={editBarkConfig.includeBalance}
                        onChange={(e) => setEditBarkConfig({ ...editBarkConfig, includeBalance: e.target.checked })}
                      />
                      <span>Hiển thị số dư sau giao dịch</span>
                    </label>
                  </div>
                </div>
              )}

              <div className="flex justify-end gap-2 pt-3 border-t border-stone-100">
                <button
                  type="button"
                  onClick={() => setEditingChannel(null)}
                  className="px-4 py-2 rounded-xl text-xs font-semibold text-stone-600 hover:bg-stone-100 transition cursor-pointer"
                >
                  Hủy
                </button>
                <button
                  type="submit"
                  disabled={isUpdating}
                  className="px-4 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition cursor-pointer"
                >
                  {isUpdating ? 'Đang lưu...' : 'Lưu thay đổi'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {/* Rotate Secret Modal */}
      {rotatingChannel && (
        <div className="fixed inset-0 z-50 bg-stone-900/40 backdrop-blur-xs flex items-center justify-center p-4">
          <div className="bg-white rounded-2xl border border-stone-200 shadow-xl max-w-md w-full p-6 space-y-4 animate-in zoom-in-95">
            <div className="flex items-center justify-between border-b border-stone-100 pb-3">
              <h3 className="text-base font-bold text-stone-900 flex items-center gap-2">
                <Key className="w-4 h-4 text-amber-600" />
                <span>Đổi khóa: {rotatingChannel.name}</span>
              </h3>
              <button
                type="button"
                onClick={() => setRotatingChannel(null)}
                className="text-stone-400 hover:text-stone-600"
              >
                ✕
              </button>
            </div>

            <form onSubmit={handleConfirmRotate} className="space-y-4">
              {rotatingChannel.provider === 'BARK' ? (
                <div>
                  <label className="block text-xs font-semibold text-stone-700 mb-1">
                    Bark Device Key mới
                  </label>
                  <input
                    type="password"
                    value={newDeviceKeyInput}
                    onChange={(e) => setNewDeviceKeyInput(e.target.value)}
                    placeholder="Dán Device Key mới từ ứng dụng Bark"
                    required
                    className="w-full px-3 py-2 rounded-xl border border-stone-200 text-sm font-mono focus:outline-none"
                  />
                  <p className="text-2xs text-stone-500 mt-1">
                    Device Key cũ sẽ được thu hồi ngay lập tức. Các thông báo sau đó sẽ gửi tới key mới này.
                  </p>
                </div>
              ) : (
                <p className="text-xs text-stone-600 leading-relaxed">
                  Khóa bí mật HMAC hiện tại sẽ bị thu hồi và một khóa mới ngẫu nhiên sẽ được sinh ra. Bạn sẽ cần cập nhật khóa mới này trên webhook receiver.
                </p>
              )}

              <div className="flex justify-end gap-2 pt-3 border-t border-stone-100">
                <button
                  type="button"
                  onClick={() => setRotatingChannel(null)}
                  className="px-4 py-2 rounded-xl text-xs font-semibold text-stone-600 hover:bg-stone-100 transition cursor-pointer"
                >
                  Hủy
                </button>
                <button
                  type="submit"
                  disabled={isRotating}
                  className="px-4 py-2 rounded-xl text-xs font-semibold bg-stone-900 text-white hover:bg-stone-800 transition cursor-pointer"
                >
                  {isRotating ? 'Đang cập nhật...' : 'Xác nhận đổi khóa'}
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  );
};
