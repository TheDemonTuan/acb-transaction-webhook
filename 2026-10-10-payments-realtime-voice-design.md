# Thiết kế đề xuất: payOS + SePay realtime và voice tiếng Việt

Ngày nghiên cứu: 10/10/2026. Trạng thái: bản thiết kế và kế hoạch triển khai đề xuất; chưa sửa repository, chưa đo production, chưa chạy test của ứng dụng.

Repository: `TheDemonTuan/acb-transaction-webhook`.
Snapshot đọc: `0ef7100d5ee9ebfff0661560db325e4ddeff8485`.
Các phát hiện bên dưới dựa trên những file đã đọc tại snapshot này; bản đồ triển khai cũng nêu các file liên quan cần kiểm tra lại trước khi chỉnh. Đường dẫn có nhãn “tạo mới” là đề xuất. Khi bắt đầu thực hiện phải đối chiếu HEAD mới và không ghi đè thay đổi mới của người dùng.

## 1. Quyết định chính

Giữ Go gateway + singleton worker + SQLite WAL/FULL + SSE + React và tts-gateway. Dọn có mục tiêu, không viết lại toàn hệ thống. Không thêm Redis, Kafka, RabbitMQ, Kubernetes, PostgreSQL, framework payment tổng quát hoặc model TTS nặng mặc định.

Tách hai phần triển khai: (A) tiền, webhook, realtime và cleanup; (B) voice nhất quán, asset-first và câu nói. Hai phần dùng hợp đồng credit event hiện tại, không bắt thay đổi provider hoặc bỏ QR tĩnh.

SePay Store/Telegram và payOS không phải cùng một nghiệp vụ: SePay ghi nhận chuyển khoản; payOS quản lý đơn và xác nhận thanh toán theo điều kiện riêng. Dùng chung sự kiện hiển thị không có nghĩa là cho SePay tự đánh dấu đơn payOS đã trả tiền.

Giữ luồng QR tĩnh: khách quét và tự nhập số tiền. Không ép tạo đơn/số tiền trước cho mọi giao dịch. Không bắt nâng gói SePay; native webhook chỉ bật khi tài khoản/gói đang sử dụng thật sự hỗ trợ và đã cấu hình.

## 2. Phát hiện từ code

F1. ACB runtime phần lớn đã bị loại: `internal/httpapi/removed_routes_test.go` kiểm tra các endpoint connection/login/OTP/poll/history cũ không còn hoạt động và không được sửa lịch sử. Vì vậy không lập kế hoạch “xóa toàn bộ poller” như thể repo chưa chuyển đổi.

F2. `internal/httpapi/payments.go:PaymentCommitNotifier` publish trước rồi gọi wake RPC đồng bộ với context 2 giây. Các service SePay/payOS gọi callback này trước khi handler trả về. Worker không phản hồi có thể kéo dài ACK; không được suy diễn đây là 2 giây chậm SSE ở mọi request, vì publish đã chạy trước.

F3. `internal/httpapi/realtime_coordinator.go` đã có hàng đợi 256, enqueue không chặn và journal recovery. `internal/httpapi/sse.go` đã có Last-Event-ID, replay, heartbeat, flush và write deadlines. Giữ những bảo đảm này.

F4. `cmd/worker/main.go` hiện publish những commit do worker tạo vào worker realtime hub. Gateway webhook publish ở gateway của nó. Cần kiểm thử hai gateway/đổi slot để xác định cửa sổ phụ thuộc journal reconciliation 1 giây; không coi in-memory hub là broadcast xuyên process.

F5. `internal/storage/sepay.go` đã commit evidence/receipt/credit/journal cùng transaction, chống trùng update/message/reference và không sửa payOS order. Bảng `connections` còn được SePay dùng; xóa bảng theo tên cũ sẽ làm hỏng nghiệp vụ mới.

F6. `internal/storage/storage.go` hiện dùng WAL, synchronous(FULL), busy_timeout 5000 và pool 4 connections. Không hạ durability để làm benchmark đẹp. Mutex writeMu là theo Store/process, không thay được tính đúng đắn giữa gateway/worker độc lập.

F7. `web/src/realtime/RealtimeDomainBridge.tsx` đã gọi voice khi nhận credit và cập nhật cache giao dịch. Nó còn invalidate nhiều query cho mỗi event. Tối ưu phần refresh phụ, không đợi refetch mới phát hiện giao dịch và không dùng generic credit làm authority của trạng thái PAID.

F8. Voice có nhiều đường cấu hình: server `VoiceSettings` lưu provider/voice/fallback; trình duyệt lưu voice/rate/pitch/template riêng. Request có thể override voice server. Engine có online-to-browser fallback. Đây là nguồn gây sai kỳ vọng cấu hình cần sửa; chưa có log để kết luận tình huống cụ thể nào đã xảy ra trên máy người dùng.

F9. `tts-gateway/app/providers/edge.py` gọi dịch vụ online qua edge-tts, không chứa local neural inference. `gtts.py` luôn lang=vi, không dùng voice nam/nữ được truyền vào, không áp dụng pitch; rate khác 0 phải qua ffmpeg. Không thể coi gTTS là giọng Hoài My/Nam Minh dự phòng tương đương.

F10. `transaction-audio-engine.ts` đã thử native audio stream; lỗi trước playback có thể gọi lại buffered request, rồi fallback trình duyệt. Không đề xuất “thêm streaming” như tính năng đang thiếu. Cần giới hạn một ngân sách retry/timeout cho toàn phiên đọc.

F11. `voice-queue.ts` dùng mảng chưa giới hạn và thêm 150ms nghỉ sau mỗi lượt. `VoiceAnnouncementProvider.tsx` có cleanup listener/leader nhưng chưa thấy cleanup queue/engine tương ứng ở unmount. Đây là rủi ro cần test, không phải bằng chứng đã có memory leak production.

F12. `tts-gateway/app/cache.py` đã có TTL, 256 items, 32MiB và key chứa text/voice/rate/pitch/mode/fallback. Không đổ lỗi “cache thiếu voice”. Cần kiểm thử cancellation producer/waiter của singleflight và giới hạn in-flight riêng; stream và buffered không dùng cùng đường coalescing.

F13. `voice-copy.ts` hiện có 6 template vui và chọn theo hash transaction ID. Giữ phong cách đó, mở rộng bằng catalog thay vì gọi LLM cho từng giao dịch.

## 3. Cleanup có giới hạn

Phân loại từng mục thành REMOVE, KEEP hoặc COMPATIBILITY. Không search-replace toàn bộ chuỗi acb.

REMOVE: tài liệu session recovery ACB đã lỗi thời ở root, hướng dẫn OTP/captcha/poll không còn đúng, code/UI/config thật sự không còn caller, generated state được track dưới `web/.wrangler`. Khi bỏ tài liệu cũ, thay bằng tài liệu payOS/SePay hiện tại; lịch sử Git vẫn lưu bản cũ.

KEEP: auth của ứng dụng, phân quyền, CSRF, keyring mã hóa dữ liệu, payments/sepay, delivery outbox, journal/replay, voice test/replay, QR tĩnh, tính đúng đắn lịch sử và migrations đã áp dụng.

COMPATIBILITY: module Go/repo/image/network/volume/router có tên acb; localStorage key đang giữ cài đặt loa; connection ID và khóa ngoại lịch sử. Không đổi để làm đẹp trong đợt sửa latency. Nếu đổi branding/localStorage thì đọc key cũ, chuyển giá trị một lần và có test không mất trạng thái mute/volume.

ACB là tên một ngân hàng hợp lệ mà payOS/SePay vẫn có thể trả về; không xóa nhãn bank=ACB như thể đó luôn là provider ACB polling cũ. Không xóa `connections` hay lịch sử tài chính. Bí mật đăng nhập ACB hết dùng có thể xóa bằng bước bảo trì có backup, phân biệt với master key đang giải mã lịch sử. Không chỉnh nội dung/checksum migration đã chạy. Không VACUUM hay mass DELETE trên đường webhook.

## 4. Hợp đồng ingress và xử lý tiền

Đường nhanh: nhận request -> xác thực -> validate/normalize -> transaction DB ngắn (dedupe + receipt/review + journal/outbox) -> local publish hint và wake hint không chặn -> ACK.

Không có network call tới Telegram, payOS, SePay, Bark hay TTS bên trong transaction DB. Không có queue delay có chủ đích trước khi ghi nhận receipt hợp lệ. Không trả success trước khi có durable acceptance; ngoại lệ là mẫu xác nhận không tài chính đã được xác minh theo hợp đồng provider.

payOS: giữ SDK signature verification, validation order/link/reference/currency/amount, confirmation sample, review và đối soát. Các nhánh cần hỏi provider thêm được đưa vào durable pending/review và worker đánh thức ngay, không bỏ kiểm tra để báo PAID sớm. Trạng thái “đã nhận callback, đang đối soát” khác “đã xác nhận thanh toán”.

SePay native: adapter riêng với JSON contract chính thức, không chế thành Telegram update giả. Dedupe theo namespace cấu hình + transaction id của SePay. HMAC dùng timestamp và raw body; timestamp chữ ký khác ngày giao dịch. API key chỉ dùng khi đó là cơ chế được cấu hình rõ ràng, không chấp nhận request không auth. ACK thành công là HTTP 200 và {"success":true}, không dùng lại {"ok":true} của Telegram.

SePay Telegram: giữ secret header và allowlist bot/chat/topic, original text only, từ chối forwarded/replied/media/unknown message classes theo chính sách hiện tại, edited message vào review. update_id không đủ cho business dedupe; giữ message và bank reference. Không gọi Telegram API lại cho từng tiền vào. Kiểm tra bot nhận đã bật Bot-to-Bot Communication Mode và có admin hoặc Group Privacy Mode tắt để nhận tin thường từ bot SePay; dùng tin thật trong group/topic đúng để chứng minh delivery. Docs bot-to-bot/features hiện nêu ngoại lệ này trong khi FAQ cũ vẫn nói bot không thấy bot, nên không dựa riêng vào FAQ hoặc fixture giả. Duy trì activation/observe rules để bật nguồn mới không đọc lại tiền cũ.

Hai transport SePay: mỗi store chọn một nguồn tài chính chính, nguồn còn lại observe trong cutover. Chỉ liên kết khi stable reference đã được chứng minh bằng fixture thực; không dedupe theo số tiền + phút nhận. Hai provider cùng nhìn một tiền vào cần phân vùng receiving account/VA hoặc ánh xạ reference đáng tin. Không bật hai nguồn active chồng lấn khi chưa chứng minh chống ghi nhận hai lần. Giữ riêng notificationAccountNumber với tài khoản/VA hiển thị trên QR.

## 5. Realtime và tài nguyên

Giữ local SSE immediate và journal là nguồn phục hồi. Thay RPC wake đồng bộ bằng tín hiệu coalesced bounded, một vòng xử lý có lifecycle. Một channel capacity=1 đủ cho “có việc mới”, không chứa mọi giao dịch. Không tạo goroutine không giới hạn cho mỗi webhook.

Để gateway cũ trong blue-green nhận được commit tại gateway mới, dùng worker realtime connection hiện hữu. Một RPC nội bộ đánh thức journal publisher; publisher đọc journal theo cursor rồi fanout. Không gửi payload tài chính tùy ý qua RPC thay cho DB authority. Nếu test chứng minh không có topology nhiều gateway đồng thời thì không bắt thêm relay chỉ để phòng xa. Vẫn giữ fallback recovery khi worker/stream hỏng.

Không rút poll interval xuống 5–10ms để giả realtime. Không bỏ heartbeat, replay hay retry đối soát. Coalesce refresh status/overview/list totals khoảng 250ms ở frontend; không coalesce chậm credit flash/chime. Keep query pages bounded. Chỉ refetch order liên quan; không gán PAID từ một credit thiếu bằng chứng đơn.

SSE slow consumers phải có giới hạn và reconnect/replay, không giữ transaction DB mở khi chờ client. Cancellation đóng HTTP body, reader, async generator, timer và audio source. Caches có cả byte/count/TTL; lịch sử giao dịch không phải RAM cache.

## 6. Một cấu hình giọng có hiệu lực

Mở rộng VoiceSettings hiện tại, không tạo một settings framework mới. Profile có revision, provider, voiceId, rate, pitch, style/catalog revision, amount bands, chime option và fallback policy. Server là authority; thiết bị chỉ giữ enabled/mute/volume/announceWhenHidden và dữ liệu cache/leader.

Chính sách mặc định đề xuất cho đọc giao dịch: FIXED_VOICE. Đây là tên mô tả hành vi; với provider Edge hiện tại dùng lại EDGE_ONLY, không thêm enum/framework mới chỉ để đổi tên. Không tự đổi sang gTTS hoặc Web Speech khi lỗi. Browser voice chỉ là mode người dùng chọn rõ; profile lỗi hiện degraded, không giả rằng cấu hình vẫn đúng.

Mỗi lần đọc snapshot profile. Không sửa giọng/tốc độ giữa câu. Rate/pitch áp dụng một nơi; không vừa synthesize rate vừa đổi playbackRate. Trong trường hợp asset pack chưa sẵn cho profile mới, UI báo pending và profile đang active; không phát asset cũ nhưng hiển thị là giọng mới.

Public chỉ được đọc profile rút gọn không chứa secret, không được tạo TTS tùy ý cho bất cứ text nào. Voice route lấy số tiền từ transaction đã lưu và validate phraseId/style/band. Test/replay phải đi cùng resolver cấu hình với luồng thật. Dùng lại voice.settings.changed với payload public allowlist an toàn để cập nhật revision.

## 7. Voice asset-first, không chờ container cho mọi lần đọc

Mục tiêu là loại network synthesis khỏi đường thường gặp, không chỉ giảm vài trăm ms trong container.

Đường mặc định mong muốn: chime local ngắn -> phần thông báo/số tiền cùng giọng -> câu vui phù hợp. Tiền phải được đọc rõ trước phần vui dài. Nạp sẵn chime và các clip cần thiết của profile đã chọn khi người dùng bật loa.

Ưu tiên nguyên cụm/số tiền trọn vẹn đã cache để tự nhiên nhất. Chuẩn bị trước số tiền hay gặp và các câu mở/kết đã duyệt. Không sinh 10.000 câu nhân với mọi số tiền. Với số tiền mới: prototype ngân hàng cụm số tiếng Việt có giới hạn, tuân theo money_vi.go/money-to-vietnamese hiện có, không ghép từng chữ hoặc từng chữ số. Test các trường hợp không trăm, linh/lẻ, mốt, lăm, nghìn/triệu/tỷ và nhóm 000. Chỉ kích hoạt number-pack khi kiểm thử transcript và nghe A/B đạt yêu cầu.

Nếu ghép cụm không đủ tự nhiên: giữ amount-full-phrase cache và online streaming cùng voice cho cold miss. Khi đó chime có ngay nhưng không hứa số tiền chưa từng gặp cũng tức thì; đây là giới hạn phải công bố trong UI/metrics, không che bằng âm loading kéo dài. Không đánh đổi phát âm đúng lấy con số latency.

Dùng Web Audio clock để schedule các buffer đã sẵn; không dựa vào nhiều setTimeout nối file MP3. Chuẩn hóa loudness/sample rate, trim leading silence cẩn thận và fade tại biên im lặng, không crossfade đè phụ âm/số tiền. Giữ compressed assets ở cache/storage có quota; chỉ decode working set, không giữ 10.000 AudioBuffer trong RAM.

Chuông dài mặc định 250ms, không tạo một sleep 250ms rồi mới bắt đầu tải voice. Tải/chuẩn bị chạy song song từ lúc event đến. Loading bổ sung nếu được bật không quá 1 giây; hết budget thì báo voice chậm/lỗi trong UI. Không phát success chime cho callback chưa xác minh/review. Một chime không được tính là đọc tiền thành công.

Giữ tts-gateway để tạo/prewarm asset và xử lý câu tùy chỉnh/cold miss. Giới hạn đề xuất ban đầu: 2 synthesis đang chạy, tối đa 16 waiting requests, không unbounded task map. Tái sử dụng cache 32MiB/256 mục hiện có; kiểm thử waiter cancellation không hủy producer của người khác. Deadline toàn phiên đọc phải chặn retry stream -> buffered -> provider khác kéo dài; chỉ một cold request cùng voice, lỗi sau playback không đọc lại từ đầu tự động.

Không thêm Azure/VieNeu mặc định. Azure Speech là lựa chọn online chính thức có tài liệu streaming và connection reuse; ARM64/image compatibility cần xác minh riêng trước chọn SDK. VieNeu-TTS v3 Turbo là ứng viên benchmark offline/pre-render, không lấy số đo Core i5/RTX của tác giả làm số đo Oracle ARM. Nếu dùng nguồn giọng khác thì tạo profile mới có tên rõ, không dùng làm fallback âm thầm.

## 8. Catalog câu vui và khung tiền

Bảo toàn 6 câu hiện tại như preset classic. Các band mặc định đề xuất, chủ tài khoản sửa được: (0, 50.000), [50.000, 200.000), [200.000, 1.000.000), [1.000.000, không giới hạn). Đơn vị VND nguyên; hai band không chồng lấn, không hở ngoài chủ ý. Mọi số tiền nhận vẫn được thông báo; lỗi catalog rơi về câu chuẩn, không làm mất tiếng đọc amount.

Catalog lưu JSON có version; mỗi câu/cụm có id, style, band/tags và weight. Dùng generator offline để tạo và duyệt dữ liệu; runtime không gọi LLM. Mục tiêu đợt đầu ít nhất 3.000 câu ghép hợp lệ độc nhất, mở rộng lên 10.000 sau duyệt. Không tính thay số tiền là một câu mới. Không cam kết 10.000 câu được viết thủ công riêng biệt.

Random chỉ chọn câu, không chọn giọng/rate/pitch. Lựa chọn có seed từ transactionId + profile revision + catalog version; lưu recipe đã chọn trong vòng đời event để rerender không đổi. Recent ring tối đa 32 phraseIds để hạn chế lặp liền, có thể chọn deterministic candidate kế tiếp. Backend validate phraseId theo band, format amount từ DB. Replay có nhãn dùng cấu hình hiện hành; không cần lưu lịch sử audio vô hạn.

Giới hạn nội dung: không chê tiền nhỏ, không gọi giới tính/tên khách khi không biết, không nói về nợ/giàu nghèo, không tục/nhạy cảm, không đọc mã chuyển khoản dài mặc định. Câu vui phải rõ nghĩa và không làm khách khó chịu.

Ví dụ nội dung mới để minh họa, không phải catalog đã triển khai:
- Đã nhận {amount}. Nhỏ xinh mà niềm vui không nhỏ!
- Đã nhận {amount}. Ví vừa nở một nụ cười!
- Đã nhận {amount}. Cửa hàng được tiếp thêm vitamin vui!
- Đã nhận {amount}. Xin phép cảm ơn hết công suất!

## 9. Hàng đợi và browser lifecycle

Credit UI độc lập với voice queue. Khi queue rảnh, bắt đầu chuẩn bị ngay. Khi đang đọc, prefetch tối đa một message kế tiếp. Không cắt mất số tiền đang nói để chạy câu mới. Bỏ khoảng nghỉ 150ms cố định khi không cần; schedule khoảng thở theo clip thực tế.

Đặt queue tối đa 20 giao dịch và giới hạn tuổi tự đọc 30 giây, cấu hình được. Khi backlog tăng thì bỏ bớt đuôi vui trước; summary nhiều giao dịch là tùy chọn rõ ràng, không bật âm thầm thay chế độ đọc từng món. Nếu overflow và summary tắt, ghi trạng thái voice skipped/overflow, giữ mọi giao dịch trên màn hình và cho replay; không giữ queue vô hạn.

Dedup của tiền, SSE và audio là ba lớp khác nhau. Audio dedupe theo transaction/event, chỉ một leader mỗi origin/browser profile; không hứa chỉ một thiết bị toàn cửa hàng nếu chưa có thiết kế device ownership. Khi mất leadership, unmount, tắt loa hoặc đổi route: cancel queue/requests/timers, release reservations và close/disconnect tài nguyên thuộc engine đúng vòng đời. Kiểm thử React StrictMode.

Cần thao tác bật loa/unlock audio theo chính sách autoplay. Không hứa chạy khi tab/app đã bị hệ điều hành đóng/suspend. Client nhận event trễ phải theo freshness policy, không đọc lại lịch sử hàng loạt khi reconnect.

## 10. Mục tiêu đo, không phải benchmark đã đạt

Trên môi trường kiểm thử ghi rõ CPU/architecture, WAL disk, network, browser và tải:
- Ingress -> durable ACK bình thường: mục tiêu p95 <= 100ms, p99 <= 250ms; loại riêng nhánh pending/review cần đối soát.
- Commit -> SSE server flush: mục tiêu p95 <= 25ms, p99 <= 100ms.
- Browser nhận event -> hiển thị/chime: mục tiêu p95 <= 50ms, p99 <= 100ms, tab foreground đã unlock.
- Browser nhận -> lời đọc có nghĩa đầu tiên với asset đã sẵn và queue rảnh: mục tiêu <= 400ms ở p95, gồm chime 250ms.
- Cold synthesis: đo riêng first audio, thời gian bắt đầu đọc amount, total latency và fallback rate; không gắn SLO warm cho cold path.

Ghi t_received, t_verified, t_committed, t_ack, t_sse_written; frontend đo bằng performance.now() cho sse/ui/chime/speech/queue. Metric hiện tên commit-to-browser nhưng đo ở server write phải được diễn giải đúng. Không trừ đồng hồ client/server rồi coi là latency mạng chính xác.

Kiểm thử mức 1, 5, 20 giao dịch/giây và burst 100 callback, bao gồm duplicate. Đây là tải kiểm thử đề xuất, không phải capacity quảng cáo. Soak staging 2 giờ + chu kỳ 100 reconnect/mount/abort; heap/goroutine/audio-node trở về vùng ổn định sau warmup/GC, không tăng tuyến tính theo số event. Không yêu cầu RSS phải lập tức trở về đúng byte ban đầu.

## 11. Rollout và ranh giới

Mỗi PR có regression tests và có thể rollback riêng. Không nâng thư viện lớn, đổi tên volume, thay deploy protocol hay frontend hosting cùng lúc. Các migration mới append-only; rollback ứng dụng không chạy down migration xóa dữ liệu.

Bắt đầu bằng số đo và bỏ RPC wait; tiếp theo xử lý cross-gateway và cleanup; voice profile và bounded lifecycle có thể triển khai độc lập; sau đó audio prototype/pack và catalog. Native SePay là nhánh có gate cấu hình, không mặc định thay Telegram. Mở đầu một quầy/thiết bị, sau đó mở rộng dựa số đo.

Không bật hai consumer đều phát âm trong rollout. Rollback audio về một fixed-voice path an toàn hoặc mute có trạng thái, không bật lại fallback đổi giọng âm thầm.

## 12. Nguồn nghiên cứu

Code: các file F1–F13 trong snapshot; URL gốc để ghép đường dẫn: `https://github.com/TheDemonTuan/acb-transaction-webhook/blob/0ef7100d5ee9ebfff0661560db325e4ddeff8485/`.

SePay payload/ACK/idempotency: `https://developer.sepay.vn/vi/sepay-webhooks/tich-hop-webhook`.
SePay HMAC raw body/timestamp: `https://developer.sepay.vn/vi/sepay-webhooks/xac-thuc`.
payOS signature: `https://payos.vn/docs/tich-hop-webhook/kiem-tra-du-lieu-voi-signature/`.
payOS webhook: `https://payos.vn/docs/du-lieu-tra-ve/webhook/`.
Telegram update/webhook secret: `https://core.telegram.org/bots/api#setwebhook`. Bot-to-bot/group delivery: `https://core.telegram.org/bots/features#bot-to-bot-communication` và `https://core.telegram.org/api/bots/bot-to-bot`; đối chiếu FAQ cũ `https://core.telegram.org/bots/faq` khi chẩn đoán khác biệt tài liệu.
SQLite WAL durability: `https://www.sqlite.org/pragma.html#pragma_synchronous`.
edge-tts upstream: `https://github.com/rany2/edge-tts`.
Azure latency guidance: `https://learn.microsoft.com/en-us/azure/ai-services/speech-service/how-to-lower-speech-synthesis-latency`.
Web Audio scheduling: `https://developer.mozilla.org/en-US/docs/Web/API/AudioBufferSourceNode/start`.
Browser autoplay: `https://developer.mozilla.org/en-US/docs/Web/Media/Guides/Autoplay`.
VieNeu upstream, chỉ tham khảo khả năng/benchmark tác giả: `https://github.com/pnnbao97/VieNeu-TTS`.

Đây là đọc code và docs. Chưa có cấu hình runtime, log/pprof, âm thanh thực tế hoặc đo trên VPS của người dùng để khẳng định root cause duy nhất, capacity hay memory leak.
