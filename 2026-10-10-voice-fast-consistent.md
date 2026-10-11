# Fast & Consistent Vietnamese Voice Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax. Đây là kế hoạch đề xuất, chưa sửa code hay production.

**Goal:** Thông báo tiền vào bằng giọng tiếng Việt đúng cấu hình, chime tức thì, hạn chế chờ tổng hợp online, có kho câu vui theo khung tiền và tài nguyên được giới hạn.

**Architecture:** Giữ credit event, VoiceAnnouncementProvider, queue và tts-gateway; chỉnh từng phần thay vì dựng hệ thống TTS mới. Một profile giọng có revision; asset cùng giọng được tạo trước, cache và schedule bằng Web Audio. Cold miss dùng một đường streaming cùng giọng, không chuyển provider âm thầm.

**Tech Stack:** Go, React/TypeScript, Web Audio, FastAPI/asyncio, edge-tts và các thư viện đã có. Không thêm model nặng, message broker hay dịch vụ TTS trả phí mặc định.

**Spec:** `../specs/2026-10-10-payments-realtime-voice-design.md`.

## Global Constraints

- Snapshot nghiên cứu: `0ef7100d5ee9ebfff0661560db325e4ddeff8485`; đối chiếu HEAD trước khi chỉnh.
- Không thay đổi số tiền, authority của giao dịch, logic settle payOS hoặc QR tĩnh.
- FIXED_VOICE là chính sách hành vi: với provider Edge hiện tại dùng lại EDGE_ONLY; không bắt thêm enum/framework provider mới chỉ để đổi tên.
- Không tự chuyển sang gTTS hoặc giọng hệ điều hành khi profile cố định gặp lỗi. Browser voice chỉ hoạt động khi được chọn rõ ràng.
- Một lần đọc dùng một snapshot profile; random câu không random giọng, tốc độ hoặc pitch.
- Test/replay/luồng thật dùng cùng resolver. Không nhân tốc độ cả ở synthesize và playback.
- Chime 250ms; chuẩn bị speech song song. Loading bổ sung tối đa 1 giây, không thay thế trạng thái lỗi hoặc metric speech.
- Queue tối đa 20 giao dịch, tuổi tự đọc 30 giây, prefetch tối đa một message; các giới hạn là cấu hình mặc định đề xuất.
- TTS tối đa 2 synthesis active và 16 waiting; cache hiện tại 32MiB/256 items được giữ trước khi có số đo.
- Kho câu: tối thiểu 3.000 biến thể hợp lệ duy nhất ở đợt đầu, có thể mở rộng 10.000; thay số tiền không được tính là biến thể mới. Giữ 6 câu classic hiện có.
- Mọi path có nhãn tạo mới là đề xuất. Không tuyên bố latency, chất lượng âm thanh hoặc test đã đạt khi chưa thực sự chạy/nghe.

## Review Focus

1. Profile vừa đổi nhưng asset cũ hoặc override localStorage vẫn phát: hiển thị pending/effective profile đúng, không giả giọng mới; V1/V4.
2. Một client hủy shared synthesis làm những client khác treo: producer/waiter cancellation tách biệt, mọi future kết thúc; V2.
3. Số tiền có nhóm 000, mốt/lăm/lẻ hoặc biên band bị đọc sai: đối chiếu transcript và nghe thật trước bật number-pack; V4/V5.
4. Tab mất leadership/unmount khi đang tải/đọc: không phát trùng, không giữ source/listener/request; V6.
5. Hàng loạt giao dịch đến nhanh hơn thời lượng câu: không chồng/cắt số tiền, queue hữu hạn, overflow có trạng thái và replay; V6/V7.

## Bản đồ file và trách nhiệm

Chỉnh có mục tiêu: `internal/storage/voice_settings.go`, `internal/httpapi/voice.go`, `internal/httpapi/sse.go`, `tts-gateway/app/main.py`, `tts-gateway/app/cache.py`, `tts-gateway/app/providers/edge.py`, `web/src/features/voice-announcements/VoiceAnnouncementProvider.tsx`, `transaction-audio-engine.ts`, `voice-engine.ts`, `voice-settings.ts`, `voice-queue.ts`, `voice-copy.ts` trong cùng thư mục feature, và `web/src/realtime/RealtimeDomainBridge.tsx`.

Tạo mới khi task cần: `voice-profile.ts`, `announcement-plan.ts`, `audio-assets.ts`, `amount-audio-plan.ts`, các test cùng feature, catalog JSON và công cụ build asset offline. Không di chuyển mọi file cũ sang một framework mới. Trước sửa formatter tiếng Việt, tìm implementation và test Go/TypeScript hiện hữu rồi tái sử dụng, không tạo thuật toán thứ ba.

---

### Task V1: Một profile giọng có hiệu lực

**Files:** chỉnh `internal/storage/voice_settings.go`, `internal/httpapi/voice.go`, `internal/httpapi/sse.go`, frontend settings/provider/engine; tạo `web/src/features/voice-announcements/voice-profile.ts` và test; thêm migration append-only sau migration cuối hiện tại.

**Interfaces đề xuất:** DTO public `VoiceProfile` gồm `revision`, `voiceId`, `rate`, `pitch`, `providerMode`, `styleId`, `catalogVersion`, `chimeEnabled`, `amountBands`, `fallbackPolicy`. `resolveVoiceProfile(settings): VoiceProfile` trả snapshot bất biến. Cấu hình thiết bị chỉ gồm enabled/mute/volume/announceWhenHidden. Tái sử dụng revision conflict và event voice.settings.changed hiện tại.

- [ ] Viết test chọn Nam Minh thì event thật/test/replay đều dùng Nam Minh; localStorage Hoài My cũ không override profile server. Test rate=1.2 chỉ áp dụng một lần, profile đổi giữa câu chỉ áp dụng cho câu kế tiếp, EDGE_ONLY lỗi không gọi gTTS/browser. Giữ test BROWSER_ONLY được người dùng chủ động chọn.
- [ ] Chạy `go test ./internal/httpapi ./internal/storage -run 'Voice' -count=1` và `cd web && bun run test:run`; xác nhận các regression mới thất bại vì hành vi cần sửa, không vì setup fixture.
- [ ] Mở rộng settings hiện tại, giữ tương thích đọc dữ liệu cũ; lần migration không tự bật loa hay đổi mute/volume. Profile realtime có authority server; API preview chỉ chấp nhận override qua quyền admin và phản hồi effective profile rõ. Public DTO/event allowlist chỉ có dữ liệu giọng an toàn, không kèm secret hoặc toàn bộ admin settings. Không fetch settings cho mỗi audio chunk.
- [ ] Chạy lại test + `bun run build`. Test public nhận profile revision mới qua event an toàn, server restart vẫn giữ profile; lỗi tải profile dùng snapshot đã xác nhận còn hợp lệ hoặc trạng thái unavailable, không random một giọng khác. Không xóa key acb.voice.settings.v1 trước khi chuyển thiết lập thiết bị một lần.
- [ ] Commit riêng profile. Viết mô tả UI phân biệt cấu hình đã lưu, profile đang có hiệu lực và asset đang chuẩn bị.

### Task V2: Rút đường fallback, giới hạn TTS và sửa cancellation

**Files:** `tts-gateway/app/main.py`, `cache.py`, `providers/edge.py`, `internal/httpapi/voice.go`, frontend `transaction-audio-engine.ts`; tạo các test `tts-gateway/tests/test_synthesis_lifecycle.py` và frontend engine test tương ứng theo test runner đã có.

**Interfaces:** giữ API streaming hiện tại. Một request voice nhận profile đã resolve; phản hồi giữ X-TTS-Provider/Voice/Fallback và metadata effective profile để quan sát. Thêm admission controller nội bộ với 2 active/16 waiting và deadline cấu hình; không tạo framework job queue.

- [ ] Viết test waiter A hủy nhưng B vẫn nhận audio; producer hủy thì mọi waiter kết thúc có lỗi thay vì future treo; stream disconnect đóng generator/upstream; Go write thất bại phải dừng đọc upstream và Close; 19 request khác key không làm active vượt 2 hoặc waiting vượt 16. Kiểm tra vượt giới hạn trả overload có kiểm soát, không bị hiểu nhầm là yêu cầu đổi giọng.
- [ ] Xác định lệnh Python test hiện hữu từ repo/CI, dùng lệnh đó chạy các test mới; không cài một runner thứ hai. Chạy `go test ./internal/httpapi -run 'Voice|Audio' -count=1` và `cd web && bun run test:run` cho phần tương ứng. Ghi failure trước sửa.
- [ ] Tách cancellation waiter khỏi shared future bằng shielding và ownership producer rõ; settle result/error/cancel trên mọi đường, remove in-flight khi kết thúc. Giữ bounded cache key đã bao gồm text/voice/rate/pitch/mode. Streaming không giữ mảng chunk vô hạn; đóng async generator trong finally. Không xây multicast streaming chỉ để có singleflight: ưu tiên coalesce request tạo asset/amount hoàn chỉnh, giữ live stream độc lập nhưng admission bounded.
- [ ] Bỏ chuỗi stream lỗi -> synthesize lại toàn bộ -> giọng khác. Mỗi announcement có một ngân sách cold request cùng voice; timeout phải cấu hình một nơi và bao phủ thời gian chờ admission. Lỗi sau playback không tự đọc lại từ đầu. Chạy test abort/error/overload và xác minh futures, tasks, thread/subprocess thuộc request kết thúc hữu hạn. Khi rollout fixed voice ổn, bỏ gTTS khỏi đường realtime; không xóa chế độ cũ trước khi xác định không còn cấu hình sử dụng.
- [ ] Commit riêng. Ghi TTFA, cancel count, queue wait, active/waiting, effective provider bằng log/metric nhẹ; không log nội dung chuyển khoản hoặc tài khoản nguyên văn. Không gọi đây là đã sửa memory leak nếu mới chứng minh cleanup bằng test.

### Task V3: Chime ngay, speech chuẩn bị song song

**Files:** tạo `web/src/features/voice-announcements/announcement-plan.ts`, test; chỉnh engine và provider hiện hữu. Asset chime là file nội bộ có quyền sử dụng rõ, không lấy nhạc không rõ giấy phép.

**Interfaces đề xuất:** `prepareAnnouncement(message: VoiceMessage, profile: VoiceProfile, signal: AbortSignal): Promise<PreparedAnnouncement>` và `playPrepared(plan: PreparedAnnouncement, signal: AbortSignal): Promise<void>` trong engine; plan chứa profile revision và các đoạn đã chuẩn bị. Không đổi credit event để mang binary audio.

- [ ] Fake AudioContext test: credit hợp lệ làm chime bắt đầu khi event tới và speech preparation được gọi trong cùng lượt xử lý, không đợi hết 250ms. Duplicate/review/không phải credit/expired không chime. Chime-ended không được đánh dấu speech thành công. Tắt loa/cancel dừng cả chime và preparation.
- [ ] Chạy `cd web && bun run test:run`; test dùng fake clock và lịch schedule, không đòi thiết bị âm thanh hay elapsed time thật trong CI.
- [ ] Dùng AudioContext clock cho các buffer sẵn. Chime mặc định 250ms; speech bắt đầu ngay sau chime nếu ready, không thêm setTimeout nghỉ. Loading tùy chọn dừng tối đa sau 1 giây, fade không đè phụ âm/số tiền. Nếu audio trễ/lỗi, credit UI vẫn hiển thị ngay và voice có trạng thái riêng. Không chime tiền vào chỉ vì nhận một callback chưa được commit/xác nhận đủ điều kiện.
- [ ] Test browser foreground đã unlock, autoplay blocked, AudioContext suspended, volume zero và replay. Ghi metric chimeStartedAt riêng speechStartedAt/amountStartedAt; không dùng chime để đạt giả mục tiêu speech dưới 400ms.
- [ ] Commit chime/lifecycle độc lập; UI không thêm animation nặng hoặc làm lại layout thu ngân đang có.

### Task V4: Asset-first và prototype đọc số tiền

**Files:** tạo `web/src/features/voice-announcements/audio-assets.ts`, `amount-audio-plan.ts` và test; tạo `tts-gateway/scripts/build_voice_pack.py` nếu không có công cụ tương đương; tạo manifest schema versioned dưới thư mục asset hiện hữu. Chỉnh API voice để resolve số tiền từ transaction DB và chỉ nhận phraseId hợp lệ.

**Interfaces đề xuất:** `planAmountAudio(amountVnd: string, profile: VoiceProfile, manifest: VoicePackManifest): AmountAudioPlan`; kết quả phân biệt full-amount-cache, number-pack hoặc same-voice-cold-stream. Manifest có version/profile hash/clipId/transcript/duration/sampleRate/content hash; không chứa thông tin khách, số tài khoản hoặc transaction ID.

- [ ] Viết fixture số tiền: 1, 5, 10, 15, 21, 25, 101, 105, 1.001, 10.005, 50.000, 99.999, 100.001, 1.000.001, 1.010.005, 1.000.000.000 và biên số tiền hợp lệ hiện tại. Transcript plan phải khớp formatter tiếng Việt hiện hữu, không floating point, không đọc chữ số riêng lẻ, không mất nhóm 000. Profile hash khác không được cache hit nhầm.
- [ ] Chạy unit tests trước. Tạo một prototype nhỏ, cùng một voice, so sánh nguyên câu online với full-amount clip và ghép cụm số. Đây là quality gate: không sinh hàng nghìn clip trước khi nghe ra cấu trúc phù hợp.
- [ ] Build offline các câu mở/kết đã duyệt và số tiền phổ biến được cấu hình; chuẩn hóa sample rate/loudness/leading silence, hash profile gồm voice/rate/pitch và asset version. Với số tiền mới, chỉ bật number-pack khi transcript đúng và nghe không ngắt khó chịu; không ghép từng chữ, không ghép file MP3 bằng nối byte tùy tiện. Nếu fail quality, dùng same-voice streaming cho cold miss và ghi giới hạn latency rõ, không giả instant cho mọi amount.
- [ ] Browser chỉ preload chime + working set của profile được chọn. Cache compressed assets theo quota, decoded buffers có byte cap, chỉ decode clip sắp phát; test eviction, disk full/cache unavailable, corrupt manifest, thiếu clip, đổi profile khi đang preload và mất mạng. Không download/decode toàn bộ 10.000 câu. Cold clip chưa sẵn không được thay bằng số tiền gần đúng hoặc giọng khác. Mỗi lần đổi rate/pitch tạo profile/asset hash mới, UI giữ đúng effective profile cho tới khi pack sẵn.
- [ ] Commit prototype đạt quality và asset path riêng; đính kèm kết quả nghe số tiền, không chỉ waveform hay ASR. Chỉ sau khi có quyền sử dụng giọng/asset rõ ràng mới phát hành pack dùng lâu dài; code mở không tự chứng minh quyền tái phân phối audio của dịch vụ online.

### Task V5: Catalog câu vui theo khung tiền

**Files:** chỉnh `web/src/features/voice-announcements/voice-copy.ts`; tạo catalog có version dưới `web/src/features/voice-announcements/catalog/`; công cụ compile offline và shared fixture giữa Go/TypeScript; mở rộng validator `internal/voicecopy` và UI settings hiện tại.

**Interfaces đề xuất:** `selectAnnouncementRecipe({ transactionId, amountVnd, profileRevision, catalogVersion, recentPhraseIds }): AnnouncementRecipe`; recipe gồm bandId, prefixId, amount slot và suffixId. Server validate recipe/phraseId thuộc catalog và amount band; không tin text/số tiền do browser tùy ý gửi lên.

- [ ] Test biên 49.999/50.000/199.999/200.000/999.999/1.000.000 đồng theo các band của spec. Test overlap/gap/negative/zero/overflow bị từ chối đúng hoặc fallback câu chuẩn; mỗi receipt hợp lệ vẫn có amount announcement. Test 6 câu classic không bị xóa, custom template cũ chỉ qua quy tắc validation đang có và có mode hiển thị rõ.
- [ ] Test selector cùng event/profile/catalog giữ cùng recipe khi rerender/retry; recent ring tối đa 32 và chọn candidate kế tiếp hữu hạn khi cần tránh lặp. Runtime không gọi model/LLM. Bộ đếm unique bỏ placeholder amount trước khi đếm, không coi 10.000 số tiền là 10.000 câu vui.
- [ ] Viết dữ liệu gốc có tags ngữ pháp/style/band và compiler chỉ ghép các cụm tương thích; sinh ít nhất 3.000 câu hợp lệ duy nhất đợt đầu, mở rộng 10.000 sau duyệt. Tách amount-core khỏi vui-tail để dùng lại audio. Cấm chê tiền nhỏ, suy đoán giới tính/tên khách, nợ/giàu nghèo, tục/nhạy cảm; giới hạn độ dài và câu vô nghĩa. Không nhân toàn catalog với mọi amount rồi synthesize.
- [ ] Chạy `go test ./internal/voicecopy ./internal/httpapi -count=1`, `cd web && bun run test:run && bun run build` và compiler validation. Lưu số câu unique thật, số combination bị loại, mẫu nghe đại diện mỗi band/style. Không ghi “10.000 câu đã có” khi mới có generator hoặc sản phẩm Cartesian chưa duyệt.
- [ ] Commit catalog + tests. UI chỉ thêm lựa chọn classic/cute/funny và khung tiền vào settings hiện tại, nút nghe thử dùng đúng recipe/profile của luồng thật. Cho phép tắt đuôi vui nhưng vẫn đọc tiền.

### Task V6: Queue hữu hạn, không phát trùng, cleanup đầy đủ

**Files:** `voice-queue.ts`, `voice-engine.ts`, `VoiceAnnouncementProvider.tsx`, engine hiện tại; chỉnh dedupe/leader nếu test chứng minh cần; tạo queue/lifecycle test cạnh file.

**Interfaces:** queue có cấu hình maxPending=20/maxAgeMs=30000/prefetch=1, trả trạng thái queued/skipped/overflow cho observer; `VoiceEngine.dispose(): void | Promise<void>` nếu interface chưa có. Dùng owner token cho request/AudioContext/source; dispose idempotent và không đóng context dùng chung của owner khác.

- [ ] Viết test 100 giao dịch liên tiếp: UI vẫn có đủ 100, queue không vượt 20, không hai amount phát chồng nhau, không cắt số tiền đang nói, chỉ prefetch một câu. Backlog chỉ lược phần vui; summary không bật nếu người dùng chưa chọn. Expired/overflow phát trạng thái skipped + có replay, không giữ reservation vô hạn.
- [ ] Viết test React StrictMode mount/unmount/remount, bật/tắt loa, leader đổi tab, stream abort, buffer đang decode, mất mạng rồi reconnect. Dedupe tiền/SSE/audio độc lập. Client cũ không được tiếp tục speak sau mất leadership, và không phát lại lịch sử thành loạt khi reconnect.
- [ ] Implement cleanup request/timer/listener/audio element/source và release/commit reservation đúng mức độ playback đã bắt đầu. Bỏ nghỉ 150ms cố định khi không cần; thay bằng clip boundary scheduling. Không giả đảm bảo exactly-once âm thanh qua crash trình duyệt: giữ chính sách chống lặp best-effort và không sửa ledger theo trạng thái loa.
- [ ] Chạy `cd web && bun run test:run && bun run build`; thêm Playwright case giả event/audio clock theo cấu hình E2E đang có. Chạy nhiều vòng lifecycle và so sánh số listeners/audio nodes/pending promises sau cleanup; không bắt RSS về đúng byte ban đầu.
- [ ] Commit queue/lifecycle. Phạm vi leader là một origin/browser profile; không thêm server device lease chỉ để đảm bảo một loa trên mọi thiết bị vì yêu cầu hiện tại chưa cần.

### Task V7: Nghiệm thu âm thanh và tài nguyên, rollout có kiểm soát

**Files:** tạo `docs/operations/voice-acceptance.md`, thêm E2E fixtures và metric nhẹ vào engine hiện tại. Không thêm monitoring stack mới.

**Interfaces:** mốc sseReceived/chimeStarted/firstMeaningfulSpeech/amountStarted/ended; queue wait, cold/warm và effective profile. Browser dùng performance.now cho khoảng trong cùng máy, không trừ clock client/server thiếu đồng bộ.

- [ ] Lập ma trận thật/test/replay, classic/cute/funny, từng amount band, giọng nam/nữ, cache warm/cold, mạng lỗi, foreground/hidden/autoplay blocked và burst. Case browser bị suspend phải báo giới hạn, không được quảng cáo chạy khi hệ điều hành đóng tab.
- [ ] Kiểm tra UI/chime p95 <=50ms từ browser event và lời có nghĩa đầu tiên p95 <=400ms với pack ready/queue idle/foreground unlocked; cold path báo riêng. Đo amountStarted để không che sự chậm bằng câu mở hoặc chuông dài. SLO này là mục tiêu spec, không phải kết quả có sẵn.
- [ ] Nghe thật từng nhóm số khó: đúng số tiền, không đổi giới tính/rate ngoài cấu hình, không im bất thường giữa chime/speech, không clipping. Soak staging 2 giờ và 100 chu kỳ reconnect/mount/cancel; theo dõi heap/goroutine/task/audio node về vùng ổn định sau warmup/GC, cache không vượt cap. Nếu tăng, profile ownership trước khi tăng RAM.
- [ ] Rollout một quầy/thiết bị, tránh hai consumer cùng phát. Rollback audio về fixed-voice an toàn hoặc mute có trạng thái, không khôi phục auto đổi giọng âm thầm. Giao dịch/SSE vẫn hoạt động khi TTS hoàn toàn ngừng.
- [ ] Commit báo cáo bằng dữ liệu thật, môi trường và những mục chưa đạt. Không triển khai model offline/Azure chỉ vì đường cold chưa đạt mục tiêu warm; đó là quyết định riêng sau phép đo và thử chất lượng.

## Thứ tự thực hiện

V1 -> V2; V3 có thể làm cùng nhịp với V2. V4 bắt đầu bằng prototype nhỏ và quality gate. V5 mở rộng catalog sau khi cách phát clip được chốt. V6 trước rollout rộng; V7 nghiệm thu. Phần core/realtime độc lập: không cần chờ catalog hoàn tất mới bỏ RPC wait.

## Nguồn và giới hạn

Đọc spec đi kèm để xem code snapshot và docs chính thức. Chưa truy cập máy production, chưa có mẫu audio người dùng, chưa chạy test ứng dụng hoặc benchmark ARM64. Artifact này là kế hoạch giao cho agent/engineer, không phải thông báo công việc đã được triển khai.
