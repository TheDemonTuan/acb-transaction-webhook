# Sổ Tay Vận Hành (Runbook): Traefik 3.x Ingress & Warm Standby Blue/Green

Tài liệu hướng dẫn quy trình chuyển dịch, triển khai và vận hành hệ thống ACB Transaction Webhook trên nền tảng đơn VPS sử dụng Traefik 3.x và mô hình Warm Standby.

---

## 1. Kiến Trúc Vận Hành

```text
Cloudflare Tunnel (172.31.250.2)
         │
         ▼
 Traefik 3.x (172.31.250.4:8080)
         │
 ┌───────┴────────────────────────┐
 │ File Provider Dynamic Route    │
 │ (Trỏ tới slot Primary hiện tại)│
 └───────┬────────────────────────┘
         │
         ▼
 ┌────────────────────────────────┐
 │ Gateway PRIMARY (Blue/Green)   │ (Đang chạy: HTTP/API/UI/SSE)
 └───────┬────────────────────────┘
         │
 ┌───────┴────────────────────────┐
 │ Gateway STANDBY (Green/Blue)   │ (Đang tắt: Giữ sẵn image & config)
 └────────────────────────────────┘
         │
    SQLite WAL
         ▲
         │
 ┌────────────────────────────────┐
 │ Worker Singleton (:8190)       │ (Đang chạy liên tục: Polling & Dispatcher)
 └────────────────────────────────┘
```

---

## 2. Quy Trình Cắt Chuyển (15-Minute Cutover Window)

1. **Khởi động Traefik Platform:**
   ```bash
   cd /opt/platform/edge   # hoặc platform/edge
   docker compose up -d
   ```
2. **Dừng Monolith cũ an toàn để nhả lock:**
   ```bash
   cd /opt/bank-event-gateway
   docker stop acb-transaction-gateway
   ```
3. **Chạy Migration một lần qua dbtool:**
   ```bash
   docker run --rm \
     --user 1000:1000 \
     -v bank-event-gateway_gateway_data:/data \
     ghcr.io/thedemontuan/acb-transaction-webhook-dbtool:latest
   ```
4. **Khởi động Worker và Core Services:**
   ```bash
   docker compose -f deploy/compose.core.yaml up -d
   ```
5. **Khởi động Gateway Blue (Primary):**
   ```bash
   SLOT=blue docker compose -p acb-blue -f deploy/compose.slot.yaml up -d
   ```
6. **Chuyển tuyến Traefik:**
   ```bash
   bash deploy/switch-slot.sh blue
   ```
7. **Khởi tạo Standby Green (Stopped):**
   ```bash
   SLOT=green docker compose -p acb-green -f deploy/compose.slot.yaml up -d
   docker compose -p acb-green -f deploy/compose.slot.yaml stop
   ```

---

## 3. Quy Trình Deploy Thông Thường (Warm Standby Release)

```bash
cd /opt/bank-event-gateway
./deploy/deploy-warm.sh ghcr.io/thedemontuan/acb-transaction-webhook@sha256:<new-digest>
```

- Hệ thống tự phát hiện slot đang chạy và kích hoạt slot đối ứng.
- Kiểm tra `/readyz` và đổi route Traefik nguyên tử.
- Giữ slot cũ chạy 15 phút (soak) để hỗ trợ rollback tức thì nếu có lỗi.
- Sau 15 phút, dừng slot cũ về trạng thái Standby.

---

## 4. Quy Trình Rollback Khẩn Cấp

Nếu phiên bản mới phát sinh lỗi, chạy lệnh sau:
```bash
bash deploy/rollback-warm.sh
```
- Standby slot cũ sẽ tự động được khởi động lại (nếu đang tắt).
- Chờ đạt `/readyz` (trong vòng 3-5 giây).
- Route Traefik lập tức được trỏ ngược về slot cũ.

---

## 5. Kích Hoạt Host Watchdog (Tự Phục Hồi <= 90s)

Cài đặt systemd timer trên máy chủ:
```bash
sudo cp deploy/host/acb-health-watchdog.service /etc/systemd/system/
sudo cp deploy/host/acb-health-watchdog.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now acb-health-watchdog.timer
```
Kiểm tra trạng thái timer:
```bash
systemctl status acb-health-watchdog.timer
```
