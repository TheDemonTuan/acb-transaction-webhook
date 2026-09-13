> **SUPERSEDED / HISTORICAL ARCHIVE**
>
> This document is retained solely for historical context, audit trails, and design lineage.
> It has been superseded by the canonical 2026-09-14 production architecture and hardening specifications:
> - **Canonical Specification:** [`docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md`](docs/superpowers/specs/2026-09-14-acb-final-production-invariants.md)
> - **Production Architecture:** [`docs/architecture/PRODUCTION_ARCHITECTURE.md`](docs/architecture/PRODUCTION_ARCHITECTURE.md)
> - **Execution Plan & Tracker:** [`docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md`](docs/superpowers/plans/2026-09-14-acb-production-convergence-execution.md)
>
> Do not implement, deploy, or operate against this document.

---

# Shared Edge Gateway Rules

Áp dụng tài liệu này cho mọi Docker Compose repository được đưa vào ingress chung trên VPS.

## Kiến trúc bắt buộc

VPS chỉ có **một edge stack** tại `/opt/edge`:

```text
Cloudflare Edge
  → một Cloudflare Tunnel dùng chung
  → một cloudflared connector stack dùng chung
  → một Caddy gateway dùng chung
  → Docker network riêng của từng ứng dụng
  → frontend HTTP tương ứng
```

Repository ứng dụng không được tự thêm Caddy hoặc `cloudflared`. Chỉ `/opt/edge` sở hữu hai service này. Mỗi hostname có một route trong cấu hình Caddy chung.

## Docker network

Mỗi ứng dụng có một network ingress riêng:

```text
edge-portfolio
edge-omniroute
edge-messenger
edge-acb
```

Không gom tất cả frontend vào một `edge-services` phẳng. Caddy chung nối vào từng network; mỗi network chỉ chứa Caddy và frontend được publish của ứng dụng đó.

Network phải được bootstrap trước với thuộc tính internal:

```bash
docker network create --internal \
  --subnet <non-overlapping-small-subnet> \
  --label io.tuan.edge.managed=true \
  edge-<app>
```

Compose của ứng dụng và edge cùng khai báo network là external. `external: true` không tự bảo đảm network là internal; preflight phải inspect và từ chối nếu thuộc tính sai.

## Compose ứng dụng

Frontend được ingress phục vụ:

```yaml
services:
  app:
    expose:
      - "8080"
    networks:
      private:
      edge:
        aliases:
          - example-web

networks:
  private:
    internal: true
  edge:
    external: true
    name: edge-example
```

Quy tắc:

1. Production không có `ports:` cho endpoint đi qua Caddy.
2. `expose:` chỉ mô tả container port; nó không publish cổng, không phải firewall và không bắt buộc cho Docker DNS.
3. Caddy sử dụng `alias:container-port`, không dùng container IP, VPS IP, `localhost` hay host port.
4. Alias phải ổn định và duy nhất trên toàn bộ network của Caddy: `<app>-web`, `<app>-api`, `<app>-ws`. Tránh tên chung như `app`, `web`, `api`.
5. Các ứng dụng có thể cùng nghe cổng 8080; Docker DNS phân biệt theo alias. Không đổi cổng nội bộ chỉ để làm chúng khác nhau.
6. Process phải listen trên interface container phù hợp, không chỉ `127.0.0.1`.
7. Chỉ frontend cần public được nối edge. DB, Redis, browser, worker, queue, vector store và admin socket chỉ ở private network.
8. Ứng dụng cần Internet outbound phải có egress network riêng; không dùng edge network để cấp Internet.
9. Debug host port chỉ được đặt trong override riêng, bind `127.0.0.1`, không tự deploy production.
10. Giữ hardening: non-root, read-only nếu hỗ trợ, drop capabilities, no-new-privileges, healthcheck, resource limit và log rotation.

Ứng dụng static không có backend có thể chỉ tham gia edge network.

## Route Caddy chung

Mỗi repository đề xuất route, nhưng deployment edge mới là nơi áp dụng route vào Caddy chung:

```caddyfile
http://app.example.com:8080 {
    bind 172.31.250.3

    reverse_proxy example-web:8080 {
        header_up X-Forwarded-Proto https
        transport http {
            dial_timeout 3s
            response_header_timeout 10s
        }
    }
}
```

Mỗi route phải xác định:

- Hostname chính xác, không wildcard mặc định.
- Network và alias upstream.
- Container port và health path.
- Phương thức HTTP, body/header limit và timeout thực tế.
- WebSocket/SSE/streaming nếu có; không áp timeout ngắn của web tĩnh cho stream.
- Public, Cloudflare Access hay machine-to-machine authentication.
- Header nhạy cảm cần loại khỏi log.
- Canary, smoke test và rollback DNS.

Unknown Host phải trả 404. Caddy admin/metrics không được publish qua hostname. Không cài plugin rate limit nếu app/Cloudflare đã đủ và chưa có nhu cầu đo được.

## IP thật và trust boundary

Caddy chỉ trust IP connector trên network riêng:

```caddyfile
trusted_proxies static 172.31.250.2/32
trusted_proxies_strict
client_ip_headers CF-Connecting-IP
```

Không trust `private_ranges` và không mặc định trust `X-Forwarded-For`. Không tự đọc `CF-Connecting-IP` trong ứng dụng khi request có thể bypass Caddy. Rate limit danh tính, API key, tài khoản, token và chi phí tiếp tục ở application/Redis.

## Cloudflare

- Một remotely-managed Tunnel publish nhiều hostname về `http://edge-caddy:8080`.
- Tunnel credential chỉ chạy connector; API token quản trị là credential khác và không mount vào connector.
- API token phải giới hạn đúng account/zone và quyền Tunnel/DNS cần thiết.
- Secret không nằm trong git, Compose, command line, log hoặc chat. Trên VPS dùng directory 0700 và file 0600.
- Admin dashboard dùng Cloudflare Access khi migrate app tương ứng. API/SDK/webhook không bật browser challenge mặc định.
- Không bật Bot Fight Mode hoặc challenge toàn zone mà chưa kiểm tra automated client hợp lệ.

## Quy trình onboarding repository

1. Khảo sát Compose/runtime thực tế, listener, health, auth, stream và private dependencies.
2. Chọn tên `edge-<app>` và alias duy nhất.
3. Thêm external edge network chỉ cho frontend.
4. Trong cửa sổ chuyển đổi, giữ rollback path có thời hạn và ghi rõ thời điểm xóa; production cuối không giữ host port hay connector riêng.
5. Thêm entry vào `/opt/edge/services.yml` và route Caddy chung.
6. Validate Compose/Caddy; kiểm tra network isolation và secret leakage.
7. Chạy hostname thử nghiệm có thời hạn hoặc probe nội bộ trước khi đổi DNS; xóa record thử nghiệm khi nghiệm thu.
8. Browser/API end-to-end, desktop/mobile nếu có UI; kiểm tra route lỗi, body/method, cache, redirect, WebSocket/SSE nếu dùng.
9. Chuyển đúng DNS hostname; theo dõi 429/5xx/latency/log.
10. Hết cửa sổ rollback, xóa connector, Tunnel, DNS thử nghiệm, token, host port, backup và marker migration của app cũ.

Không migrate nhiều ứng dụng cùng lúc. Không chạy `docker system prune`, không restart project khác và không sửa firewall/SSH trong cùng change.

## Deploy và rollback

App deploy chỉ recreate service của app, tuyệt đối không restart edge. Edge config được stage, validate rồi reload/deploy riêng. Image production dùng immutable digest.

Trước cutover phải lưu:

- DNS record và Tunnel ingress cũ.
- Compose/config đang chạy.
- Image digest hiện tại.
- Health/header/route baseline.

Rollback DNS chỉ trỏ về connector cũ sau khi xác nhận connector/upstream cũ đang healthy. App rollback phải khôi phục cả image và Compose snapshot, rồi kiểm tra health nội bộ và qua ingress.

## Checklist nghiệm thu

- [ ] Một Tunnel chung và một Caddy chung; app không có proxy/connector riêng.
- [ ] Không có host port production cho endpoint đã migrate.
- [ ] Network app là external + internal và không chứa service nội bộ không cần thiết.
- [ ] Caddy resolve alias sau khi recreate app.
- [ ] Unknown Host 404; method/body/header policy đúng.
- [ ] IP thật không spoof được từ Internet.
- [ ] Secret/header/query nhạy cảm không vào log hoặc image context.
- [ ] Health app, Caddy, Tunnel và public hostname được kiểm tra riêng.
- [ ] UI được test end-to-end desktop/mobile; API/webhook/stream có test phù hợp.
- [ ] Các stack khác giữ nguyên baseline.
- [ ] Rollback đã được ghi rõ và resource cũ chưa bị xóa quá sớm.
