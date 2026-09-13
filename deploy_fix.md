Mình đã đọc **HEAD mới nhất hiện tại** và log deploy thật của GitHub Actions. Nguyên nhân Bark deploy fail lần này đã xác định được khá chắc chắn.

Commit mới nhất là `5cefa4a` lúc khoảng **13:53 ngày 13/09/2026**, nội dung chỉ là `test(storage): stabilize auth attempt expiry timing`, tức là không sửa Bark. Workflow của chính commit này vẫn **failure ở job Deploy to VPS**.

## Root cause hiện tại

Log production mới nhất báo:

```text
acb-bark | /bark-entrypoint.sh: line 16: 1: parameter not set
```

Đây không phải healthcheck, APNs, Basic Auth hay Cloudflare.

Trong `deploy/bark-entrypoint.sh` hiện tại có:

```sh
#!/usr/bin/env sh
set -eu

...

if [ "$1" = "bark-server" ]; then
    shift
fi

...

exec "$cmd" "$@"
```

Chính `"$1"` là chỗ crash.

### Tại sao `$1` lại không tồn tại?

Compose hiện cấu hình:

```yaml
bark:
  entrypoint: ["/bin/sh", "/bark-entrypoint.sh"]
```

nhưng **không có `command:`**.

Trong image Bark chính thức, upstream Dockerfile vốn có:

```dockerfile
ENTRYPOINT ["/entrypoint.sh"]
CMD ["bark-server"]
```

Nhưng đây là chi tiết quan trọng của Docker Compose:

> Khi `entrypoint` trong Compose được set khác `null`, Compose **bỏ qua CMD mặc định của image**. ([Docker Documentation][1])

Tức là cấu hình của bạn biến:

```text
Bark image gốc

ENTRYPOINT /entrypoint.sh
CMD bark-server
```

thành:

```text
Compose của bạn

ENTRYPOINT /bin/sh /bark-entrypoint.sh
CMD <empty>
```

Kết quả:

```text
/bark-entrypoint.sh
        ↓
không có argument nào
        ↓
$1 không tồn tại
        ↓
set -u
        ↓
"1: parameter not set"
        ↓
exit
        ↓
restart: unless-stopped
        ↓
restart loop
        ↓
Docker Compose: acb-bark unhealthy
        ↓
deploy fail
```

Docker cũng xác nhận khi override entrypoint thì default command/CMD có thể bị clear. ([Docker Documentation][2])

---

## Vì sao mấy commit healthcheck vừa rồi không fix được?

Bạn vừa có chuỗi commit kiểu:

```text
b0d5a75  fix(deploy): probe Bark root endpoint
85696fc  fix(deploy): send Bark health auth header
04ad401  fix(deploy): use Bark process health check
```

Commit `04ad401` cuối cùng đổi healthcheck sang:

```yaml
healthcheck:
  test:
    ["CMD-SHELL", "pidof bark-server >/dev/null || exit 1"]
```

Nhưng vấn đề nằm **trước healthcheck**:

```text
entrypoint
   ↓
CRASH
   ↓
bark-server chưa bao giờ được exec
   ↓
healthcheck đương nhiên fail
```

Nên đổi:

```text
/ping
→ /
→ thêm Basic Auth
→ pidof
```

đều không thể chữa được lỗi này.

---

# Fix đúng

Ở `deploy/bark-entrypoint.sh`, thay:

```sh
if [ "$1" = "bark-server" ]; then
    shift
fi
```

thành:

```sh
if [ "${1:-}" = "bark-server" ]; then
    shift
fi
```

Đây là fix chính.

Vì với:

```sh
set -u
```

thì:

```sh
"$1"
```

khi không có argument → crash.

Còn:

```sh
"${1:-}"
```

khi không có argument → trả về chuỗi rỗng.

Sau đó:

```sh
cmd="bark-server"

if [ -x /usr/local/bin/bark-server ]; then
    cmd="/usr/local/bin/bark-server"
fi

exec "$cmd" "$@"
```

sẽ chạy đúng:

```text
/usr/local/bin/bark-server
```

dù script nhận **0 argument**.

---

## Mình còn khuyên thêm `command:` rõ ràng

Compose có thể viết:

```yaml
bark:
  image: ${BARK_IMAGE_REF:?BARK_IMAGE_REF must be an immutable digest}

  entrypoint:
    - /bin/sh
    - /bark-entrypoint.sh

  command:
    - bark-server
```

Khi đó:

```text
/bin/sh
   ↓
/bark-entrypoint.sh bark-server
                      ↑
                     $1
```

script:

```sh
if [ "${1:-}" = "bark-server" ]; then
    shift
fi
```

rồi:

```sh
exec /usr/local/bin/bark-server
```

### Mình khuyên làm cả hai

Không chỉ:

```yaml
command: ["bark-server"]
```

mà vẫn sửa:

```sh
${1:-}
```

Lý do là entrypoint script nên **tự an toàn khi được chạy không argument**.

---

# Có một điểm rất tốt: mấy lỗi cũ đã được sửa

Log mới cho thấy phần backup mình cảnh báo trước đó giờ đã chạy đúng:

```text
Backing up the active gateway named volume...
database backup created
destination="/backup/gateway-20260913065758.db"
```

Và Bark hiện cũng đã được pin immutable:

```text
ghcr.io/finb/bark-server@
sha256:32d65b07fa835c99b31a396b77727a04...
```

Tức là hai vấn đề lớn trước đó:

```text
❌ backup sai host path
❌ bark-server:latest
```

đã được xử lý.

Dispatcher cũng đang log:

```text
notification dispatcher started
max_retries=8
workers=4
```

nên phần worker regression trước đó cũng đã sửa.

---

# Patch mình khuyên commit ngay

`deploy/bark-entrypoint.sh`:

```sh
#!/usr/bin/env sh
set -eu

load_secret() {
    file="$1"
    variable="$2"

    [ -r "$file" ] || {
        printf 'Required Bark secret is missing or unreadable: %s\n' "$file" >&2
        exit 1
    }

    value="$(tr -d '\r\n' < "$file")"

    [ -n "$value" ] || {
        printf 'Required Bark secret is empty: %s\n' "$file" >&2
        exit 1
    }

    export "$variable=$value"
}

load_secret \
    /run/secrets/bark_basic_auth_user \
    BARK_SERVER_BASIC_AUTH_USER

load_secret \
    /run/secrets/bark_basic_auth_password \
    BARK_SERVER_BASIC_AUTH_PASSWORD

if [ "${1:-}" = "bark-server" ]; then
    shift
fi

cmd="bark-server"

if [ -x /usr/local/bin/bark-server ]; then
    cmd="/usr/local/bin/bark-server"
fi

exec "$cmd" "$@"
```

Và Compose:

```yaml
entrypoint:
  - /bin/sh
  - /bark-entrypoint.sh

command:
  - bark-server
```

Commit:

```bash
git add deploy/bark-entrypoint.sh deploy/compose.prod.yaml

git commit -m \
"fix(deploy): make Bark entrypoint safe without image CMD"
```

## Kết luận

**Lỗi deploy Bark hiện tại là do interaction giữa Docker Compose `entrypoint` + `CMD` và `set -u`, không phải do healthcheck.**

Cụ thể:

```text
Compose custom entrypoint
        ↓
default CMD "bark-server" bị bỏ
        ↓
script nhận zero arguments
        ↓
"$1" không tồn tại
        ↓
set -u làm script exit
        ↓
Bark restart loop
        ↓
deploy fail
```

Chỉ cần sửa `$1 → ${1:-}` là giải quyết root cause hiện tại; thêm `command: ["bark-server"]` giúp contract Compose rõ ràng và chống regression tốt hơn.

[1]: https://docs.docker.com/reference/compose-file/services/?utm_source=chatgpt.com "Define services in Docker Compose | Docker Docs"
[2]: https://docs.docker.com/reference/dockerfile?utm_source=chatgpt.com "Dockerfile reference | Docker Docs"
