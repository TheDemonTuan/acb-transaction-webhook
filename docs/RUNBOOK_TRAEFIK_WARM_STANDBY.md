> **SUPERSEDED / HISTORICAL ARCHIVE**
>
> This document is retained solely for historical context, audit trails, and design lineage.
> It has been superseded by the canonical 2026-09-14 production architecture and hardening specifications:
> - **Canonical Specification:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](superpowers/specs/2026-09-14-acb-final-production-invariants.md)
> - **Production Architecture:** [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](architecture/PRODUCTION_ARCHITECTURE.md)
> - **Execution Plan & Tracker:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](superpowers/plans/2026-09-14-acb-production-convergence-execution.md)
> - **Authoritative Runbook:** [`docs/runbooks/DEPLOYMENT_RUNBOOK.md`](runbooks/DEPLOYMENT_RUNBOOK.md)
> - **Failover Runbook:** [`docs/runbooks/FAILOVER_RUNBOOK.md`](runbooks/FAILOVER_RUNBOOK.md)
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

> **LƯU Ý:** Script nguyên khối cũ (`deploy-warm.sh` và cờ `--upgrade-core`) đã chính thức **BỊ BÃI BỎ VÀ HARD-FAIL**.
> Các hoạt động triển khai hiện tại bắt buộc phải sử dụng các script giao dịch thành phần độc lập hoặc `deploy/dispatch-rollout.sh`. Chi tiết đầy đủ tại [`docs/runbooks/DEPLOYMENT_RUNBOOK.md`](runbooks/DEPLOYMENT_RUNBOOK.md).

### Chế độ 1: Khởi tạo mới hoàn toàn (Fresh Initialization)
Khi thiết lập một máy chủ mới chưa có dữ liệu production:
1. **Khởi tạo Data Volumes có xác nhận an toàn:**
   ```bash
   cd /opt/acb-transaction-webhook
   ./deploy/init-fresh-data.sh --confirm-fresh-init
   ```
2. **Khởi tạo Secrets và Runtime Environment:**
   ```bash
   ./deploy/provision-secrets.sh --confirm-fresh-provision
   cp .env.example deploy/.env.production && chmod 600 deploy/.env.production
   ```
3. **Triển khai các thành phần theo thứ tự phụ thuộc:**
   ```bash
   ./deploy/deploy-schema.sh ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest>
   ./deploy/deploy-worker.sh ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest>
   ./deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
   ```

### Chế độ 2: Chuyển đổi từ Monolith cũ (Legacy Monolith Migration)
Khi nâng cấp từ container monolith `acb-transaction-gateway`:
1. **Dừng Monolith cũ để giải phóng lock SQLite:**
   ```bash
   cd /opt/acb-transaction-webhook
   docker stop acb-transaction-gateway || true
   ```
2. **Sao lưu trực tuyến và chạy Migration qua dbtool:**
   ```bash
   ./deploy/backup-db.sh
   ./deploy/deploy-schema.sh ghcr.io/thedemontuan/acb-transaction-webhook-dbtool@sha256:<dbtool-digest>
   ```
3. **Khởi động hệ thống mới với Worker và Core services:**
   ```bash
   ./deploy/deploy-worker.sh ghcr.io/thedemontuan/acb-transaction-webhook-worker@sha256:<worker-digest>
   ./deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<gateway-digest>
   ```

### Chế độ 3: Triển khai thông thường (Already-BlueGreen Warm Standby Release)
Mặc định triển khai **web-only**, TUYỆT ĐỐI KHÔNG pull hoặc restart Worker/Core singleton:
```bash
cd /opt/acb-transaction-webhook
./deploy/deploy-gateway.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<new-gateway-digest>
```
- Tự động phát hiện slot đang chạy (vd: `blue`) và triển khai lên slot standby (vd: `green`).
- Thử nghiệm candidate tại `/internal/deployz` 2 lần liên tiếp.
- Cắt chuyển tuyến động Traefik tại `/opt/edge/dynamic/acb.yml`.
- Xác nhận tích cực định danh `X-Platform-Slot: green`. Nếu thất bại, tự động rollback về slot cũ.

### Rollback Khi Cần Thiết:
```bash
cd /opt/acb-transaction-webhook
./deploy/rollback.sh
```

---

## 3. Giao Kèo Bộ Điều Khiển Trung Tâm (Central Controller CLI Contract)

Bộ điều khiển chuyển vùng sự cố (`platform/failover/vps-failover-controller.py`) giám sát trạng thái realtime của containers.

### Giao thức tương tác:
1. **Lệnh chuyển slot:**
   ```bash
   /bin/bash deploy/switch-slot.sh <blue|green>
   ```
2. **Không khóa lồng nhau & khóa tương hỗ (Mutual Host Lock):**
   `switch-slot.sh` và failover controller dùng chung tệp khóa `/run/lock/vps-failover/acb.lock`. Kiểm tra biến môi trường `DEPLOY_LOCK_HELD=1` hoặc `SKIP_LOCK=1` để không gây deadlock khi được gọi từ controller hoặc deploy script.
3. **Nhận thức Transaction Journal & Ngăn Promotion trong Transaction:**
   Failover controller kiểm tra `deploy-journal.json`. Nếu giao dịch triển khai đang chạy (`TX_INITIALIZED`, `CANDIDATE_STARTING`, `VERIFYING_HEALTH`, `SWITCHING_ROUTE`, `VERIFYING_ACK`, `TX_SOAKING`), controller tự động hoãn/chặn failover để không tranh chấp với tiến trình triển khai.
4. **Đánh dấu Intentional Stop:**
   Trước khi dừng slot cũ thành standby hoặc dừng container bảo trì, script tự động tạo marker `/var/lib/vps-failover/apps/<app>/intentional-stop-<slot>` (và `/tmp/vps-failover/intentional-stop-<slot>`) đồng thời kích hoạt cooldown. Controller hiểu đây là hành vi chủ động có chủ đích và KHÔNG kích hoạt failover giả lập.
5. **Worker Singleton & Fencing:**
   Worker (`acb-worker`) được đăng ký tại `/etc/vps-failover/apps.d/worker.json` với chính sách tự phục hồi có backoff mũ, không bao giờ tạo tiến trình worker song song (fencing).
6. **Xác thực Định Danh Tuyến (Exact Route Identity ACK):**
   Khi chuyển slot, controller yêu cầu xác nhận định danh qua header `X-Platform-Slot` và `X-Release-Commit`. Nếu xác nhận thất bại, slot standby bị rollback/dừng lại và hệ thống chuyển về chế độ cảnh báo suy giảm (degraded mode).

---

## 4. Giới Hạn Kiến Trúc: Không Có HA Khi Mất Host (No Host-Level HA)

Hệ thống được thiết kế tối ưu trên **đơn VPS**. Vì vậy:
- **Không có tính sẵn sàng cao khi sập toàn bộ máy chủ (Host Failure):** Nếu VPS bị tắt nguồn, hỏng ổ cứng hoặc mất mạng, toàn bộ hệ thống sẽ ngừng hoạt động cho đến khi được khôi phục.
- **RPO (Recovery Point Objective):** Bằng chu kỳ sao lưu cơ sở dữ liệu gần nhất (khuyến nghị sao lưu định kỳ mỗi 15-30 phút thông qua cron job gọi `deploy/backup-db.sh`).
- **RTO (Recovery Time Objective):** Dưới 15 phút nếu có máy chủ dự phòng (cold standby) và thực hiện theo [`docs/runbooks/DISASTER_RECOVERY_RUNBOOK.md`](runbooks/DISASTER_RECOVERY_RUNBOOK.md).

---

## 5. Danh Sách Kiểm Tra Khi Sự Cố (Troubleshooting Checklist)

1. **Traefik báo 502 / Bad Gateway:**
   - Kiểm tra container gateway đang active: `docker ps --filter name=acb-gateway`.
   - Xem cấu hình động Traefik: `cat /opt/edge/dynamic/acb.yml`.
   - Kiểm tra log Traefik: `docker logs --tail 50 traefik`.
2. **Worker không lấy được giao dịch mới:**
   - Kiểm tra log worker: `docker logs --tail 100 acb-worker`.
   - Kiểm tra auth session: truy cập `/api/auth/status`. Nếu hết hạn, dùng `cmd/auth-browser` để đăng nhập lại.
3. **Rollback thủ công khẩn cấp:**
   - Thực thi `./deploy/rollback.sh`.
