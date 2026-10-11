# Payment Core Cleanup & Realtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax. Đây là kế hoạch đề xuất, chưa phải phê duyệt sửa production.

**Goal:** Dọn ACB runtime còn sót, làm webhook payOS/SePay phản hồi nhanh và phát sự kiện ngay mà không mất tính đúng đắn tài chính.

**Architecture:** Giữ gateway/worker/SQLite/SSE. Commit là điểm xác nhận durable; publish và wake là tín hiệu phục hồi được. Native SePay dùng adapter riêng, không trộn vào Telegram hoặc nghiệp vụ settle payOS.

**Tech Stack:** Go, modernc SQLite, chi, React, TanStack Query, SSE; các phiên bản đang có trong repo, không nâng major.

**Spec:** `../specs/2026-10-10-payments-realtime-voice-design.md`.

## Global Constraints

- Snapshot nghiên cứu: `0ef7100d5ee9ebfff0661560db325e4ddeff8485`; đối chiếu HEAD trước khi thực hiện.
- Không sửa deploy/network/volume identity hoặc thay database/message broker.
- Không xóa lịch sử, không sửa checksum migration đã áp dụng; WAL/FULL được giữ.
- Không ACK success trước durable acceptance; sample xác nhận provider là ngoại lệ không tài chính đã xác minh.
- SePay credit không tự settle payOS order; giữ static QR và hai loại tài khoản SePay notification/QR riêng.
- Không gọi external API/TTS trong transaction; không goroutine không giới hạn theo webhook.
- Mọi test/path có nhãn tạo mới là đề xuất; không báo đã pass khi chưa thực sự chạy.

## Review Focus

1. Callback commit xong nhưng worker treo: ACK không chờ, không mất credit; Task C2.
2. Client ở slot A, callback vào slot B: event tới A không chờ tick thường lệ trong đường khỏe; Task C3.
3. Bật native + Telegram hoặc payOS trên cùng nguồn tiền: không nhân đôi và không match bằng amount/time; Task C6.
4. Xóa ACB nhưng `connections`/migrations còn được dùng: lịch sử và SePay không hỏng; Task C4.
5. Signature hợp lệ nhưng callback không đủ điều kiện settle: lưu pending/review, không chime/PAID giả; Task C1/C5.

## Bản đồ file

Giữ và chỉnh có mục tiêu:
`cmd/gateway/main.go`, `cmd/worker/main.go`, `internal/httpapi/payments.go`, `internal/httpapi/realtime_coordinator.go`, `internal/httpapi/sse.go`, `internal/payments/webhook_service.go`, `internal/payments/reconcile.go`, `internal/sepay/service.go`, `internal/sepay/telegram.go`, `internal/storage/sepay.go`, `internal/storage/settlement.go`, `internal/workerrpc/rpc.go`, `web/src/realtime/RealtimeDomainBridge.tsx`.

Tạo mới theo task: wake_signal.go, journal_publisher.go, test regression, adapter SePay native khi gate đạt. Không di chuyển các file đang ổn vào một framework mới.

---

### Task C1: Đóng băng hợp đồng và đo baseline

**Files:** tạo `internal/httpapi/payment_latency_test.go`, `internal/storage/payment_ingest_invariants_test.go`; mở rộng telemetry hiện hữu và fixture test hiện tại. Đọc `internal/httpapi/removed_routes_test.go` trước khi thêm test trùng.

**Interfaces:** giữ nguyên credit event và `PaymentCommitNotifier() func(storage.EventNotification)`. Chỉ thêm mốc received/verified/committed/ack/sse_written và request ID an toàn; không ghi raw tài khoản hoặc secret vào log.

- [ ] Thêm test `TestReceiptDurableBeforeSuccess`, `TestConcurrentDuplicateCreatesOneCredit`, `TestProviderSampleDoesNotCreateCredit`, `TestReviewDoesNotPublishCredit`; assertions là số receipt/credit/journal/outbox thực, không chỉ HTTP 200.
- [ ] Chạy `go test ./internal/httpapi ./internal/storage ./internal/payments ./internal/sepay -count=1`; lưu kết quả baseline. Test bảo toàn đã pass được giữ làm regression; test phát hiện thiếu observability/bug phải có kết quả thất bại đúng nguyên nhân trước sửa.
- [ ] Bổ sung histogram giai đoạn vào telemetry hiện có; tách server SSE write khỏi browser arrival. Không đổi đường xử lý tiền để lấy metric.
- [ ] Chạy lại cùng lệnh; ghi benchmark proposal cho 1/5/20 callback mỗi giây và burst 100 vào báo cáo staging. Không chạy tải thật vào tài khoản ngân hàng.
- [ ] Commit riêng phần test/metrics; ghi điều kiện môi trường, không báo SLO đã đạt nếu mới unit test.

### Task C2: Wake không chặn đường ACK

**Files:** tạo `internal/workerrpc/wake_signal.go`, `internal/workerrpc/wake_signal_test.go`; chỉnh bootstrap `cmd/gateway/main.go`, hợp đồng/comment và test `internal/httpapi/payments.go`, test nhánh wake của payments.

**Interfaces đề xuất:** `NewWakeSignal(fn func(context.Context) error, timeout time.Duration) *WakeSignal`; `(*WakeSignal).Wake(context.Context) error` chỉ enqueue không chặn; `(*WakeSignal).Run(context.Context)` là loop có lifecycle. Dùng chữ ký Wake hiện tại để inject vào `WithWakeDispatcher` và `WithReconcileWake`, không nhân đôi code timeout/retry cho mỗi handler.

- [ ] Viết `TestWakeSignalCoalesces`, `TestWakeSignalStops`, `TestWebhookAckDoesNotWaitForWorker`, `TestReconcileWakeDoesNotBlockAck`. Fake RPC giữ ở một channel chưa release; handler phải trả success sau commit trước khi channel được release. Không dùng network thật hoặc test thời gian flakey.
- [ ] Chạy `go test ./internal/workerrpc ./internal/httpapi ./internal/payments -run 'Wake|AckDoesNotWait' -count=1`; trước sửa phải thể hiện fake RPC chặn đường đang test.
- [ ] Implement channel capacity=1, một runner mỗi loại wake, timeout RPC 2 giây trong runner, context cancel để shutdown. Không dùng `go fn()` trên mỗi webhook. Inject callback enqueue-only; bỏ sự phụ thuộc ACK vào RPC dù call site cũ còn hợp đồng context. Mất hint phải được durable polling/journal cứu.
- [ ] Chạy toàn bộ package tests và `go test -race ./internal/workerrpc ./internal/httpapi ./internal/payments`; burst 10.000 wake hints không làm goroutine/request tăng tuyến tính và không thay đổi số credit.
- [ ] Commit riêng. So sánh ACK khi worker khỏe/treo; bảo đảm SSE publish vẫn sau DB commit.

### Task C3: Realtime xuyên gateway/blue-green

**Files:** tạo `internal/realtimestream/journal_publisher.go`, `internal/realtimestream/journal_publisher_test.go`, `internal/httpapi/realtime_multigateway_test.go`; chỉnh `cmd/worker/main.go`, `cmd/gateway/main.go`, `internal/workerrpc/rpc.go`. Giữ coordinator và SSE replay hiện hữu.

**Interfaces:** `NewJournalPublisher(store *storage.Store, hub *eventhub.Hub) *JournalPublisher`; `Wake()` nonblocking; `Run(ctx context.Context)`. RPC nội bộ `WakeCommittedEvents(ctx context.Context) error` chỉ đánh thức publisher và dispatcher, không nhận payload credit để tự tin tưởng. Gateway gọi qua WakeSignal của C2.

- [ ] Trước khi code relay, viết test hai Server/Store độc lập chia sẻ DB: client kết nối A, receipt commit B. Chặn periodic reconciliation trong test để quan sát riêng live path; event phải tới A và đúng thứ tự. Thêm duplicate hints, worker restart, full coordinator queue, stale Last-Event-ID.
- [ ] Chạy `go test ./internal/httpapi ./internal/realtimestream ./internal/workerrpc -run 'MultiGateway|JournalPublisher|Replay' -count=1`. Nếu kiến trúc mới ở HEAD đã đáp ứng live relay, giữ test và không tạo relay thứ hai.
- [ ] Nếu còn gap, implement cursor reader bounded batch, init trước ready, wake coalesced và fanout qua worker realtime stream đã tồn tại. Giữ fallback recovery. Không mở DB transaction qua thời gian SSE/client chờ. Event cũ/duplicate do coordinator xử lý, không tạo financial rows từ hint.
- [ ] Chạy `go test -race ./internal/httpapi ./internal/realtimestream ./internal/workerrpc`; staging blue-green với SSE còn ở old slot. Đo server flush và client receipt riêng, xác minh reconnect không đọc lại voice lịch sử.
- [ ] Commit riêng. Không sửa deploy protocol, không giảm poll interval xuống vài ms.

### Task C4: Dọn ACB theo inventory, không phá dữ liệu

**Files:** `.env.example`, `.gitignore`, hai tài liệu `2026-09-15-acb-session-recovery*.md`, UI/copy/config không còn caller; tạo `docs/architecture/payment-runtime.md` và `scripts/check-legacy-runtime.py`; mở rộng `internal/httpapi/removed_routes_test.go`.

**Interfaces:** inventory file/identifier với ba quyết định REMOVE, KEEP, COMPATIBILITY. Static check chỉ cấm business entry point/import/copy đã bỏ, không cấm chữ acb trong module path, migration, volume, test.

- [ ] Viết các assertions cho route/login/OTP/poll cũ không hoạt động, không có runtime outbound polling ACB; thêm kiểm tra new checkout không còn track `web/.wrangler` runtime state. Fixture migration chứa ACB history và một SePay receipt.
- [ ] Chạy kiểm tra trên HEAD để lập inventory; phân biệt bug thật với compatibility hợp lệ. Không “sửa” test bằng xóa dữ liệu fixture hoặc nới allowlist vô hạn.
- [ ] Xóa code/UI/config dead đã chứng minh, thay docs bằng runtime payOS/SePay, bỏ generated state khỏi Git index và ignore. Giữ ứng dụng auth, keyring, `connections`, lịch sử và tất cả migration đã áp dụng. Branding chỉ đổi phần hiển thị cần thiết. Secret ACB hết dùng đưa vào thao tác bảo trì có backup, không xóa master key.
- [ ] Chạy `go test ./...`, `cd web && bun run test:run && bun run build`; kiểm tra database nâng cấp giữ counts/hash tài chính. Rà `git diff --stat` và `git diff` để chắc không đụng deploy identity ngoài scope.
- [ ] Commit riêng cleanup; không trộn với logic money hoặc tuning âm thanh.

### Task C5: Fast ingress và UI, giữ pending/review đúng nghĩa

**Files:** `internal/payments/webhook_service.go`, `internal/payments/reconcile.go`, `internal/storage/settlement.go`, `web/src/realtime/RealtimeDomainBridge.tsx`; tạo `internal/payments/webhook_deferred_confirmation_test.go`, `web/src/realtime/RealtimeDomainBridge.coalescing.test.tsx`.

**Interfaces:** giữ `bank.transaction.credit` chỉ cho credit đã được nghiệp vụ chấp nhận. Existing pending/review/outbox được dùng cho nhánh cần provider confirmation, không tạo một event bus mới. UI vẫn refetch authoritative order khi credit không có đủ trạng thái settle.

- [ ] Thêm test late callback cho CANCELLED/EXPIRED, mismatch receiver/currency/amount/reference, provider offline. Assertion: callback có bằng chứng được giữ để xử lý lại nhưng không báo PAID/chime sai. UI test 100 credit unique chỉ thay first page trong giới hạn limit, duplicate không cộng totals hai lần.
- [ ] Chạy `go test ./internal/payments ./internal/storage -run 'Webhook|Settlement|Late' -count=1` và `cd web && bun run test:run`; dùng fake provider đếm outbound calls để pin chỗ phải deferred.
- [ ] Giữ validate/signature trước acceptance. Nhánh phải gọi provider chuyển sang durable pending rồi đánh thức reconciler; không biến pending thành success tài chính. UI hiển thị/voice từ event ngay; chỉ coalesce invalidation status/overview/summary 250ms. Không debounce credit flash và không bỏ refetch cần để khẳng định payOS order.
- [ ] Chạy tests, race tests backend, browser integration với burst/reconnect và in-flight snapshot. Đo query count, không đặt blanket retry hoặc refresh interval cho mọi màn hình.
- [ ] Commit riêng; xác minh UI event-to-flash cải thiện mà trạng thái order không suy diễn.

### Task C6: Native SePay adapter và source authority, có gate

**Files:** tạo `internal/sepay/webhook.go`, `internal/sepay/webhook_test.go`, `internal/httpapi/sepay_webhook.go`, `internal/httpapi/sepay_webhook_test.go`; mở rộng `internal/storage/sepay.go`, managed config và router hiện có. Migration mới được cấp số append-only tại lúc triển khai, không giả định số 019 còn trống sau HEAD mới.

**Gate:** chỉ bật production nếu tài khoản SePay thực có native webhook và đã cấu hình auth/source authority. Telegram tiếp tục dùng được; không bắt nâng gói. Nếu gate không đạt, giữ adapter disabled và không thay flow đang chạy.

**Interfaces:** `VerifyNativeWebhook(rawBody []byte, headers http.Header, secret string, now time.Time) (NativeWebhook, error)`; `NativeWebhook` giữ provider id/reference/account/subAccount/amount/date chính xác. `IngestSePayWebhook(ctx, input)` dùng inbox riêng của native transport và phần ghi receipt dùng chung trong SePay; không giả botId/updateId.

- [ ] Viết fixtures HMAC đúng/sai, whitespace khác, timestamp quá hạn, Unicode, số nguyên lớn, id duplicate đồng thời, same id altered amount, outgoing, wrong receiver, empty referenceCode. Test response đúng HTTP 200 + {"success":true}; JSON {"ok":true} không được dùng cho native.
- [ ] Thêm test hai transport cùng reference đã chứng minh chỉ tạo một credit khi authority cho phép; hai reference khác cùng amount/time phải là hai giao dịch. Không có reference đủ tin thì secondary observe/review, không fuzzy merge.
- [ ] Implement theo docs SePay raw-body timestamp signature, body limit, integer parsing, bank/timezone validation. Chọn nguồn active theo store/account/VA, giữ riêng notificationAccountNumber và QR receiver; sample chuyển cấu hình không được bật lại old events. Public không lộ token/HMAC secret.
- [ ] Chạy `go test -race ./internal/sepay ./internal/storage ./internal/httpapi`. Kiểm thử sandbox/native khi có credentials test; Telegram dùng fixture và controlled message vì môi trường test có thể không gửi notification giống live. Không tạo giao dịch thật tự động.
- [ ] Commit riêng, enable bằng cấu hình có kiểm tra và cutover observe->active; không bật hai nguồn overlapping active để “so sánh tốc độ”.

### Task C7: Fault tests, soak và rollout

**Files:** tạo test/load fixtures và `docs/operations/realtime-acceptance.md`; không thêm monitoring stack mới chỉ để chạy bài này.

**Interfaces:** metric schema từ C1 và runtime contracts từ C2–C6.

- [ ] Tạo fault cases DB busy/disk failure, crash ngay sau commit trước ACK, worker down, duplicate retry, gateway restart, slow SSE client, channel full. Pin expected financial counts và eventual recovery.
- [ ] Chạy package tests/race, sau đó staging load 1/5/20 events mỗi giây + burst 100 và soak 2 giờ; lưu các kết quả đạt/chưa đạt riêng warm/cold và healthy/degraded.
- [ ] So sánh mục tiêu spec; nếu chậm, profile CPU/heap/DB lock trước khi đổi architecture. Không disable FULL/auth/dedupe hoặc tăng mọi timeout.
- [ ] Rollout một quầy/nguồn, test blue-green và rollback ứng dụng bằng quy trình đang có. Ledger/migrations không bị rollback phá dữ liệu; SSE replay vẫn hoạt động.
- [ ] Commit acceptance report với lệnh, môi trường, ngày và kết quả thật; không claim không leak chỉ bằng quan sát container chạy một lần.

## Telegram delivery acceptance bắt buộc

Trước bật nguồn SePay Telegram active, kiểm tra Bot-to-Bot Communication Mode của bot nhận; có admin hoặc privacy mode tắt để nhận tin bot thông thường. Thử một notification thật vào đúng group/topic, kiểm tra getWebhookInfo/pending updates/lỗi gần nhất và nhận đúng SenderBotID. HTTP 200 từ fixture không chứng minh Telegram sẽ chuyển tin thật. Giữ parser original-text và allowlist; không nới tin forward/reply chỉ để làm smoke test thành công. Không tự trả lời bot SePay tạo vòng lặp. Các bước cấu hình là runbook/control plane, không gọi Bot API cho mỗi giao dịch.

## Handoff

Triển khai C1 -> C2 -> C3/C4 -> C5; C6 chỉ qua gate; C7 nghiệm thu. Plan voice có thể làm độc lập với credit event hiện tại. Không tự thực thi khi người dùng mới yêu cầu kế hoạch.
