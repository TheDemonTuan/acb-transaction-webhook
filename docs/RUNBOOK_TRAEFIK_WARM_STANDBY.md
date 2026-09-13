> **SUPERSEDED / HISTORICAL ARCHIVE**
>
> This document is retained solely for historical context, audit trails, and design lineage.
> It has been superseded by the canonical 2026-09-14 production architecture and hardening specifications:
> - **Canonical Specification:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](superpowers/specs/2026-09-14-acb-final-production-invariants.md)
> - **Production Architecture:** [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](architecture/PRODUCTION_ARCHITECTURE.md)
> - **Execution Plan & Tracker:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](superpowers/plans/2026-09-14-acb-production-convergence-execution.md)
>
> Do not implement, deploy, or operate against this document.

---

# Sổ Tay Vận Hành (Runbook): Traefik 3.x Ingress & Warm Standby Blue/Green

Tài liệu hướng dẫn quy trình chuyển dịch, triển khai và vận hành hệ thống ACB Transaction Webhook trên nền tảng đơn VPS sử dụng Traefik 3.x, Docker Compose hợp nhất (`compose.prod.yaml`) và mô hình Warm Standby Blue/Green.

---

## 1. Kiến Trúc Vận Hành

```text
Cloudflare Tunnel (172.31.250.2)
         │
         ▼
 Traefik 3.x (172.31.250.4:8080)
         │
 ┌───────┴────────────────────────────────────────┐
 │ File Provider Dynamic Route (/opt/edge/dynamic)│
 │ (Trỏ nguyên tử tới slot Primary hiện tại)      │
 └───────┬────────────────────────────────────────┘
         │
         ▼
 ┌────────────────────────────────┐
 │ Gateway PRIMARY (Blue/Green)   │ (Đang chạy: HTTP/API/UI/SSE trên :8090)
 └───────┬────────────────────────┘
         │
 ┌───────┴────────────────────────┐
 │ Gateway STANDBY (Green/Blue)   │ (Đang tắt: Giữ sẵn image & config)
 └────────────────────────────────┘
         │
    SQLite WAL (Volume: bank-event-gateway_gateway_data)
         ▲
         │
 ┌────────────────────────────────┐
 │ Worker Singleton (:8190)       │ (Đang chạy liên tục: Polling & Dispatcher)
 └────────────────────────────────┘
         │
 ┌───────┴────────────────────────┐
 │ Sidecars (Auth-Browser, TTS,   │
 │ Bark Push Notification Server) │
 └────────────────────────────────┘
```

---

## 2. Quy Trình Cắt Chuyển và Các Chế Độ Triển Khai

Hệ thống hỗ trợ 3 chế độ vận hành chính thông qua file Compose hợp nhất `deploy/compose.prod.yaml`:

### Chế độ 1: Khởi tạo mới hoàn toàn (Fresh Initialization)
Khi thiết lập một máy chủ mới chưa có dữ liệu production:
1. **Khởi tạo Data Volumes có xác nhận an toàn:**
   ```bash
   cd /opt/bank-event-gateway
   ./deploy/init-fresh-data.sh --confirm-fresh-init
   ```
2. **Khởi động Platform Ingress:**
   ```bash
   cd /opt/platform/edge
   docker compose up -d
   ```
3. **Triển khai toàn bộ hệ thống lần đầu:**
   ```bash
   cd /opt/bank-event-gateway
   ./deploy/deploy-warm.sh --upgrade-core ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
   ```

### Chế độ 2: Chuyển đổi từ Monolith cũ (Legacy Monolith Migration)
Khi nâng cấp từ container monolith `acb-transaction-gateway`:
1. **Khởi động Traefik Platform:**
   ```bash
   cd /opt/platform/edge
   docker compose up -d
   ```
2. **Dừng Monolith cũ để giải phóng lock SQLite:**
   ```bash
   cd /opt/bank-event-gateway
   docker stop acb-transaction-gateway || true
   ```
3. **Sao lưu trực tuyến và chạy Migration qua dbtool:**
   ```bash
   ./deploy/backup.sh
   docker run --rm --user 1000:1000 \
     -v bank-event-gateway_gateway_data:/data:rw \
     ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest> \
     -path /data/gateway.db -migrate
   ```
4. **Khởi động hệ thống mới với Worker và Core services:**
   ```bash
   ./deploy/deploy.sh --upgrade-core \
     ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest> \
     ghcr.io/thedemontuan/acb-transaction-webhook-auth-browser@sha256:<browser-digest> \
     ghcr.io/thedemontuan/acb-transaction-webhook-tts-gateway@sha256:<tts-digest>
   ```

### Chế độ 3: Triển khai thông thường (Already-BlueGreen Warm Standby Release)
Mặc định triển khai **web-only**, TUYỆT ĐỐI KHÔNG pull hoặc restart Worker/Core singleton:
```bash
cd /opt/bank-event-gateway
./deploy/deploy-warm.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<new-gateway-digest>
```
- Tự động phát hiện slot đang chạy (vd: `blue`) và kích hoạt slot đối ứng (`green`).
- Kiểm tra `/readyz` và đổi route Traefik nguyên tử.
- Xác nhận route identity ACK.
- Giữ slot cũ chạy trong cửa sổ 15 phút (soak) để hỗ trợ rollback tức thì nếu có lỗi.
- Đánh dấu intentional stop TRƯỚC KHI dừng container cũ về trạng thái Standby.

---

## 3. Giao Kèo Bộ Điều Khiển Trung Tâm (Central Controller CLI Contract)

Bộ điều khiển chuyển vùng sự cố (`/opt/platform/failover/vps-failover-controller.py`) giám sát trạng thái realtime của containers.

### Giao thức tương tác:
1. **Lệnh chuyển slot:**
   ```bash
   /bin/bash deploy/switch-slot.sh <blue|green>
   ```
2. **Không khóa lồng nhau (No Nested Lock):**
   `switch-slot.sh` kiểm tra biến môi trường `DEPLOY_LOCK_HELD=1` hoặc `SKIP_LOCK=1` để không gây deadlock khi được gọi từ controller hoặc deploy script.
3. **Đánh dấu Intentional Stop:**
   Trước khi dừng slot cũ thành standby, script tự động tạo marker `/tmp/vps-failover/acb.intentional-stop` và kích hoạt cooldown. Controller sẽ hiểu đây là hành vi chủ động có chủ đích và KHÔNG kích hoạt failover giả lập.

---

## 4. Giới Hạn Kiến Trúc: Không Có HA Khi Mất Host (No Host-Loss HA)

**Lưu ý an toàn quan trọng:**
Kiến trúc đơn VPS với Warm Standby Blue/Green cung cấp khả năng tự phục hồi và triển khai không downtime đối với **lỗi phần mềm trên máy chủ** (container crash, OOM, deploy release).

Kiến trúc này **KHÔNG** cung cấp High Availability khi mất hoàn toàn máy chủ vật lý (Host-Loss):
- Nếu máy chủ VPS bị mất điện, hỏng phần cứng hoặc mất mạng diện rộng, toàn bộ dịch vụ sẽ ngừng hoạt động.
- Quy trình khắc phục thảm họa (Disaster Recovery):
  1. Khởi tạo một máy chủ VPS mới.
  2. Khôi phục dữ liệu từ bản sao lưu mã hóa ngoài máy chủ (`manifest-*.json` và bản sao lưu SQLite được xuất qua `ENCRYPTED_BACKUP_HOOK`).
  3. Khôi phục các khóa bí mật trong thư mục `secrets/` (`app_master_key`).
  4. Triển khai lại stack qua lệnh Chế độ 1 (Fresh Initialization).

---

## 5. Quy Trình Rollback Khẩn Cấp

Nếu phiên bản mới phát sinh lỗi, chạy lệnh sau:
```bash
bash deploy/rollback-warm.sh
```
- Standby slot cũ sẽ tự động được khởi động lại (nếu đang tắt).
- Chờ đạt `/readyz` (trong vòng 3-5 giây).
- Route Traefik lập tức được trỏ ngược về slot cũ.
- Slot lỗi được dừng về standby an toàn.

---

## 6. Kích Hoạt Failover Controller & Reconcile Safety Net

Cài đặt systemd services cho failover controller trên máy chủ:
```bash
sudo cp platform/failover/vps-failover-controller.service /etc/systemd/system/
sudo cp platform/failover/vps-failover-reconcile.service /etc/systemd/system/
sudo cp platform/failover/vps-failover-reconcile.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now vps-failover-controller.service
sudo systemctl enable --now vps-failover-reconcile.timer
```
Kiểm tra trạng thái dịch vụ:
```bash
systemctl status vps-failover-controller.service
systemctl status vps-failover-reconcile.timer
```
