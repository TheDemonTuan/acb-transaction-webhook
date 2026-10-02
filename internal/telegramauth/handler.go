package telegramauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/challenge"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type ReplyBroker interface {
	HandleReply(ctx context.Context, chatID, promptID, incomingID int64, text string) error
}
type BrowserCanceller interface {
	Cancel(ctx context.Context, attemptID string) error
}
type RecoveryScheduler interface {
	ScheduleRecovery(ctx context.Context, connectionID string, generation int64, eventKey string) error
}
type HandlerOptions struct {
	Store          *storage.Store
	Client         *Client
	ChatID, UserID int64
	PublicOrigin   string
	ReplyBroker    ReplyBroker
	Browser        BrowserCanceller
	Scheduler      RecoveryScheduler
	AIDegraded     func() string
}
type Handler struct{ HandlerOptions }

func NewHandler(options HandlerOptions) (*Handler, error) {
	if options.Store == nil || options.Client == nil || options.ChatID == 0 || options.UserID == 0 {
		return nil, errors.New("TELEGRAM_HANDLER_CONFIG_INVALID")
	}
	u, err := url.Parse(options.PublicOrigin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("TELEGRAM_PUBLIC_ORIGIN_INVALID")
	}
	options.PublicOrigin = strings.TrimRight(options.PublicOrigin, "/")
	return &Handler{options}, nil
}
func (h *Handler) authorized(chat Chat, from *User) bool {
	return chat.Type == "private" && chat.ID == h.ChatID && from != nil && from.ID == h.UserID && !from.IsBot
}
func present(raw []byte) bool { return len(raw) != 0 && string(raw) != "null" }
func unsafeMessage(m *Message) bool {
	return m.EditDate != 0 || m.ForwardDate != 0 || present(m.ForwardOrigin) || present(m.ForwardFrom) || present(m.ForwardFromChat) || present(m.ViaBot) || present(m.SenderChat) || present(m.Photo) || present(m.Video) || present(m.Animation) || present(m.Audio) || present(m.Voice) || present(m.VideoNote) || present(m.Document) || present(m.Contact) || present(m.Sticker) || present(m.Location) || present(m.Venue) || present(m.Poll) || present(m.Dice) || present(m.Story) || present(m.PaidMedia) || present(m.Game) || present(m.Invoice) || present(m.SuccessfulPayment) || present(m.PassportData) || present(m.WebAppData)
}
func rejectedDisposition(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, storage.ErrChallengeMismatch) || errors.Is(err, storage.ErrChallengeConsumed) || errors.Is(err, storage.ErrChallengeExpired) || errors.Is(err, storage.ErrRecoverySuperseded) || errors.Is(err, storage.ErrRecoveryCommitted) || errors.Is(err, storage.ErrRecoveryCooldown) || errors.Is(err, storage.ErrRecoveryBudgetExhausted) || errors.Is(err, storage.ErrAuthAttemptActive) || errors.Is(err, storage.ErrRecoveryRunInvalidState) || errors.Is(err, storage.ErrRecoveryNotReady)
}
func (h *Handler) HandleUpdate(ctx context.Context, u Update) error {
	if present(u.EditedMessage) {
		return nil
	}
	if u.Callback != nil {
		return h.handleCallback(ctx, u.Callback)
	}
	m := u.Message
	// No command or reply text is examined until the exact operator is authorized.
	if m == nil || !h.authorized(m.Chat, m.From) || unsafeMessage(m) {
		return nil
	}
	if m.ReplyTo != nil && m.ReplyTo.ID > 0 {
		if h.ReplyBroker == nil {
			return errors.New("TELEGRAM_REPLY_BROKER_UNAVAILABLE")
		}
		err := h.ReplyBroker.HandleReply(ctx, m.Chat.ID, m.ReplyTo.ID, m.ID, m.Text)
		if err == nil || rejectedDisposition(err) || errors.Is(err, challenge.ErrInvalidResponse) || errors.Is(err, challenge.ErrOutcomeUnknown) {
			return nil
		}
		return err
	}
	switch m.Text {
	case "/start", "/help":
		return h.say(ctx, "Bot riêng để khôi phục đăng nhập ACB. Chỉ trả lời CAPTCHA/OTP khi bot yêu cầu; không nhập OTP chuyển tiền. Telegram không mã hóa đầu cuối; xóa tin chỉ là cố gắng tốt nhất.\n/acb_status — trạng thái\n/acb_login — xác nhận đăng nhập\n/acb_retry — xác nhận thử lại\n/acb_pause — tạm dừng tự đăng nhập (không dừng theo dõi)\n/acb_resume — tiếp tục tự đăng nhập\n/acb_cancel — xác nhận hủy lần này\n/acb_manual — xác nhận chuyển sang trang quản trị")
	case "/acb_status":
		return h.status(ctx)
	case "/acb_pause":
		return h.direct(ctx, "PAUSE")
	case "/acb_resume":
		return h.direct(ctx, "RESUME")
	case "/acb_login":
		return h.confirm(ctx, "LOGIN")
	case "/acb_retry":
		return h.confirm(ctx, "RETRY")
	case "/acb_cancel":
		return h.confirm(ctx, "CANCEL")
	case "/acb_manual":
		return h.confirm(ctx, "MANUAL")
	default:
		return h.say(ctx, "Lệnh không hợp lệ. Gửi /help để xem hướng dẫn; trả lời trực tiếp tin yêu cầu khi nhập CAPTCHA hoặc OTP.")
	}
}
func (h *Handler) say(ctx context.Context, text string) error {
	_, err := h.Client.SendText(ctx, h.ChatID, text, nil)
	return err
}
func (h *Handler) snapshot(ctx context.Context) (storage.Connection, storage.AuthRecoveryEpisode, error) {
	c, err := h.Store.Connection(ctx)
	if err != nil {
		return c, storage.AuthRecoveryEpisode{}, err
	}
	e, err := h.Store.LatestAuthRecoveryEpisode(ctx, c.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return c, storage.AuthRecoveryEpisode{}, nil
	}
	if err != nil {
		return c, e, err
	}
	if e.Generation != c.Generation {
		e = storage.AuthRecoveryEpisode{}
	}
	return c, e, nil
}

type inlineButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type inlineKeyboard struct {
	Rows [][]inlineButton `json:"inline_keyboard"`
}

func (h *Handler) buttons(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, operations []string) (inlineKeyboard, []storage.TelegramAuthAction, error) {
	var keyboard inlineKeyboard
	var actions []storage.TelegramAuthAction
	for _, operation := range operations {
		a, err := h.Store.CreateTelegramAuthAction(ctx, storage.TelegramAuthAction{BotID: h.Client.Readiness().BotID, ChatID: h.ChatID, UserID: h.UserID, EpisodeID: e.ID, ExpectedGeneration: c.Generation, Action: operation})
		if err != nil {
			return keyboard, actions, err
		}
		actions = append(actions, a)
		keyboard.Rows = append(keyboard.Rows, []inlineButton{{Text: actionLabel(operation), Data: "ar:" + a.ID}})
	}
	return keyboard, actions, nil
}
func actionLabel(operation string) string {
	switch operation {
	case "LOGIN":
		return "Xác nhận đăng nhập"
	case "RETRY":
		return "Xác nhận thử lại"
	case "CANCEL":
		return "Xác nhận hủy lần này"
	case "MANUAL":
		return "Xác nhận chuyển manual"
	case "PAUSE":
		return "Tạm dừng"
	case "RESUME":
		return "Tiếp tục"
	default:
		return "Trạng thái"
	}
}
func (h *Handler) sendButtons(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, text string, operations []string) (int64, error) {
	keyboard, actions, err := h.buttons(ctx, c, e, operations)
	if err != nil {
		return 0, err
	}
	id, err := h.Client.SendText(ctx, h.ChatID, text, keyboard)
	if err != nil {
		return 0, err
	}
	for _, a := range actions {
		if err := h.Store.DeliverTelegramAuthAction(ctx, a.ID, id); err != nil {
			return id, err
		}
	}
	return id, nil
}
func (h *Handler) confirm(ctx context.Context, operation string) error {
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	if operation == "LOGIN" && c.State == "MONITORING" {
		blocked, err := h.Store.HasBlockingAuthRecovery(ctx, c.ID, c.Generation)
		if err != nil {
			return err
		}
		if !blocked {
			return h.say(ctx, "Phiên đang hoạt động.")
		}
		return h.say(ctx, "Phiên đã xác minh, đang chờ bù giao dịch. Dùng /acb_retry nếu bước bù đã thất bại; không đăng nhập lại.")
	}
	if (operation == "CANCEL") && e.RecoveryRunID != "" && c.State == "MONITORING" {
		return h.say(ctx, "Không thể hủy bước bù đã bắt đầu; phiên và cổng chặn được giữ nguyên. Dùng /acb_pause để ngăn lần đăng nhập tiếp.")
	}
	if operation == "LOGIN" || operation == "RETRY" {
		report, err := h.Store.ActiveAuthAttempts(ctx)
		if err != nil {
			return err
		}
		for _, a := range report.Attempts {
			if a.OwnerSubject != storage.RecoveryOwner {
				return h.say(ctx, "Đang có phiên đăng nhập manual. Bot chờ, không chiếm hoặc hủy phiên này.")
			}
		}
		if e.AttemptID != "" {
			ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
			if err == nil {
				expiry, parseErr := time.Parse(time.RFC3339Nano, ch.ExpiresAt)
				if parseErr == nil && time.Now().Before(expiry) {
					return h.say(ctx, "Đang chờ yêu cầu CAPTCHA hoặc OTP còn hiệu lực. Hãy trả lời trực tiếp yêu cầu đó.")
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
	}
	_, err = h.sendButtons(ctx, c, e, fmt.Sprintf("%s? Kết nối %s, đợt %s. Nút xác nhận dùng một lần, hết hạn sau 60 giây.", actionLabel(operation), shortID(c.ID), shortID(e.ID)), []string{operation})
	return err
}
func (h *Handler) direct(ctx context.Context, operation string) error {
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	disposition, err := h.Store.ApplyTelegramAuthOperation(ctx, h.Client.Readiness().BotID, e.ID, c.Generation, operation)
	if err != nil {
		if rejectedDisposition(err) {
			return h.say(ctx, operationFailure(err))
		}
		return err
	}
	return h.afterOperation(ctx, e, operation, disposition)
}
func (h *Handler) handleCallback(ctx context.Context, q *CallbackQuery) error {
	if q.Message == nil || q.InlineMessageID != "" || !h.authorized(q.Message.Chat, &q.From) || unsafeMessage(q.Message) {
		return nil
	}
	// Answer even stale/unknown authorized callbacks to clear the spinner. This is
	// best effort and independent of the durable command disposition.
	defer func() { _ = h.Client.AnswerCallbackQuery(ctx, q.ID) }()
	if !strings.HasPrefix(q.Data, "ar:") || len(q.Data) != 25 {
		return h.say(ctx, "Nút không hợp lệ hoặc đã hết hạn. Gửi lại lệnh để lấy nút mới.")
	}
	a, disposition, err := h.Store.ConsumeTelegramAuthAction(ctx, strings.TrimPrefix(q.Data, "ar:"), h.Client.Readiness().BotID, h.ChatID, h.UserID, q.Message.ID, time.Now())
	if err != nil {
		if rejectedDisposition(err) {
			return h.say(ctx, operationFailure(err))
		}
		return err
	}
	var e storage.AuthRecoveryEpisode
	if a.EpisodeID != "" {
		e, err = h.Store.AuthRecoveryEpisode(ctx, a.EpisodeID)
		if err != nil {
			return err
		}
	}
	return h.afterOperation(ctx, e, a.Action, disposition)
}
func operationFailure(err error) string {
	switch {
	case errors.Is(err, storage.ErrRecoveryCommitted):
		return "Không thể hủy bước bù hoặc thử lại dữ liệu cần sửa thủ công. Phiên và cổng chặn vẫn được giữ nguyên."
	case errors.Is(err, storage.ErrRecoveryCooldown):
		return "Chưa đủ 60 giây từ lần gửi đăng nhập trước. Hãy chờ rồi xác nhận lại."
	case errors.Is(err, storage.ErrAuthAttemptActive):
		return "Đang có phiên đăng nhập. Hãy chờ, bot không chiếm phiên manual."
	default:
		return "Yêu cầu đã dùng, hết hạn hoặc trạng thái đã thay đổi. Gửi /acb_status rồi lấy nút xác nhận mới."
	}
}
func (h *Handler) afterOperation(ctx context.Context, e storage.AuthRecoveryEpisode, operation, disposition string) error {
	if (operation == "PAUSE" || operation == "CANCEL" || operation == "MANUAL") && e.AttemptID != "" && e.RecoveryRunID == "" && h.Browser != nil {
		_ = h.Browser.Cancel(ctx, e.AttemptID)
	}
	switch disposition {
	case "STATUS":
		return h.status(ctx)
	case "CATCHUP_RETRY":
		if h.Scheduler == nil {
			return errors.New("TELEGRAM_RECOVERY_SCHEDULER_UNAVAILABLE")
		}
		run, err := h.Store.GetRecoveryRun(ctx, e.RecoveryRunID)
		if err != nil {
			return err
		}
		if err := h.Scheduler.ScheduleRecovery(ctx, e.ConnectionID, e.Generation, run.EventKey); err != nil {
			return err
		}
		return h.say(ctx, "Đã yêu cầu thử lại bù giao dịch với phiên hiện tại; không đăng nhập hoặc yêu cầu OTP lại.")
	case "ACTIVE":
		return h.say(ctx, "Phiên đang hoạt động.")
	case "REARMED":
		return h.say(ctx, "Đã xác nhận đợt đăng nhập. Ngân sách mới đã được cấp; nếu đang tạm dừng hãy dùng /acb_resume.")
	case "PAUSE":
		return h.say(ctx, "Đã tạm dừng tự đăng nhập, yêu cầu đang chờ đã bị hủy. Theo dõi với phiên đã xác minh và bù giao dịch vẫn tiếp tục.")
	case "RESUMED":
		return h.say(ctx, "Đã tiếp tục tự đăng nhập. Không đặt lại ngân sách hoặc trạng thái cần manual; dùng /acb_retry để xác nhận nếu cần.")
	case "MANUAL":
		return h.say(ctx, "Đã tạm dừng tự đăng nhập. Mở trang quản trị HTTPS để đăng nhập manual: "+h.PublicOrigin+"/admin")
	case "CANCEL":
		return h.say(ctx, "Đã hủy lần này. Bot không tự tạo lại đợt đã hủy; dùng /acb_login hoặc /acb_retry để xác nhận đợt mới.")
	default:
		return errors.New("TELEGRAM_OPERATION_INVALID")
	}
}
func (h *Handler) status(ctx context.Context) error {
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	bot, err := h.Store.TelegramAuthState(ctx, h.Client.Readiness().BotID)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Kết nối %s: %s.\nĐợt %s: %s. Số lần thử: %d (ngân sách hiện tại %d/3).\nTạm dừng tự đăng nhập: %t.", shortID(c.ID), connectionLabel(c.State), shortID(e.ID), episodeLabel(e.State), e.AttemptCount, e.AttemptCount-e.BudgetStartCount, bot.Paused)
	if e.AttemptID != "" {
		ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
		if err == nil {
			text += "\nĐang chờ " + challengeLabel(ch.Kind) + "; hết hạn " + localExpiry(ch.ExpiresAt) + "."
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if e.RecoveryRunID != "" {
		run, err := h.Store.GetRecoveryRun(ctx, e.RecoveryRunID)
		if err != nil {
			return err
		}
		text += fmt.Sprintf("\nBù giao dịch: %s; khoảng %s đến %s; ngày tiếp theo %s.", episodeLabel(string(run.Status)), run.RangeFrom, run.RangeTo, run.NextDay)
	}
	settings, err := h.Store.GetMonitorSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		text += "\nLịch theo dõi hiện đang tạm dừng."
	}
	ready := h.Client.Readiness()
	if !ready.Ready {
		text += "\nTelegram: kết nối suy giảm."
	} else {
		text += "\nTelegram: sẵn sàng."
	}
	if h.AIDegraded != nil && h.AIDegraded() != "" {
		text += "\nAI CAPTCHA: suy giảm; dùng phản hồi người vận hành."
	}
	return h.say(ctx, text)
}
func connectionLabel(state string) string {
	switch state {
	case "MONITORING":
		return "phiên đã xác minh"
	case "AUTH_REQUIRED":
		return "cần đăng nhập"
	case "AUTH_STARTING":
		return "đang đăng nhập"
	case "UNCONFIGURED":
		return "chưa cấu hình"
	default:
		return "chưa sẵn sàng"
	}
}
func challengeLabel(kind string) string {
	if kind == "OTP" {
		return "OTP đăng nhập"
	}
	return "CAPTCHA"
}
func episodeLabel(state string) string {
	switch state {
	case "DETECTED":
		return "đã phát hiện mất phiên"
	case "STARTING", "LOGIN":
		return "đang đăng nhập"
	case "WAITING_CAPTCHA":
		return "chờ CAPTCHA"
	case "WAITING_OTP":
		return "chờ OTP đăng nhập"
	case "VERIFYING":
		return "đang xác minh"
	case "CATCHING_UP", "RUNNING", "PENDING":
		return "đang bù giao dịch"
	case "RETRY_WAIT":
		return "chờ thử lại"
	case "WAIT_OPERATOR":
		return "chờ xác nhận người vận hành"
	case "MAINTENANCE_WAIT":
		return "chờ ngân hàng bảo trì"
	case "MANUAL_REQUIRED", "FAILED":
		return "cần xử lý thủ công"
	case "COMPLETED":
		return "hoàn tất"
	case "CANCELLED", "CANCELED":
		return "đã hủy"
	case "SUPERSEDED":
		return "đã được thay thế"
	default:
		return "không có đợt hiện tại"
	}
}
func reasonLabel(reason string) string {
	switch reason {
	case "CREDENTIALS_REJECTED":
		return "Thông tin đăng nhập bị từ chối"
	case "ACCOUNT_LOCKED":
		return "Tài khoản bị khóa"
	case "CATCHUP_FAILED", "HISTORY_RANGE_UNAVAILABLE":
		return "Chưa bù đủ giao dịch; cổng theo dõi vẫn bị chặn"
	case "INVALID_CHECKPOINT":
		return "Mốc lịch sử không hợp lệ, cần sửa có kiểm soát"
	case "SESSION_DECRYPT_FAILED":
		return "Không giải mã được phiên, cần kiểm tra khóa mã hóa"
	case "ACCOUNT_SELECTION_REQUIRED":
		return "Không chọn được đúng tài khoản"
	case "UNSUPPORTED_CHALLENGE":
		return "Ngân hàng yêu cầu phương thức không hỗ trợ; mở ứng dụng hoặc đăng nhập manual"
	case "MANUAL_ACTIVE":
		return "Đang có phiên manual"
	default:
		return "Cần kiểm tra trạng thái và xác nhận bước tiếp theo"
	}
}

// DeliverNotices is at-least-once: a crash between send and persistence may
// duplicate a notice, never a bank action. Retry times survive controller restart.
func (h *Handler) DeliverNotices(ctx context.Context) error {
	notices, err := h.Store.PendingAuthRecoveryNotices(ctx)
	if err != nil {
		return err
	}
	for _, n := range notices {
		if n.NextAttemptAt != "" {
			when, err := time.Parse(time.RFC3339Nano, n.NextAttemptAt)
			if err != nil {
				return errors.New("TELEGRAM_NOTICE_RETRY_INVALID")
			}
			if time.Now().Before(when) {
				continue
			}
		}
		e, err := h.Store.AuthRecoveryEpisode(ctx, n.EpisodeID)
		if err != nil {
			return err
		}
		c, err := h.Store.Connection(ctx)
		if err != nil {
			return err
		}
		text, err := h.noticeText(ctx, n, e)
		if err != nil {
			return err
		}
		// Never offer actions fenced to an old generation. The historical notice
		// remains truthful but cannot affect a newer connection.
		var messageID int64
		if e.Generation == c.Generation && e.ConnectionID == c.ID {
			messageID, err = h.sendButtons(ctx, c, e, text, noticeActions(n.Kind))
		} else {
			messageID, err = h.Client.SendText(ctx, h.ChatID, text, nil)
		}
		if err != nil {
			delay := 30 * time.Second
			var transport *TransportError
			if errors.As(err, &transport) && transport.RetryAfter > delay {
				delay = transport.RetryAfter
			}
			if persistErr := h.Store.FinishAuthRecoveryNotice(ctx, n.ID, 0, time.Now().Add(delay)); persistErr != nil {
				return persistErr
			}
			return err
		}
		if err := h.Store.FinishAuthRecoveryNotice(ctx, n.ID, messageID, time.Time{}); err != nil {
			return err
		}
	}
	return nil
}
func noticeActions(kind string) []string {
	switch kind {
	case "DETECTED", "STARTING", "LOGIN", "WAITING_CAPTCHA", "WAITING_OTP":
		return []string{"STATUS", "PAUSE", "CANCEL"}
	case "WAIT_OPERATOR", "MANUAL_REQUIRED", "RETRY_WAIT", "MAINTENANCE_WAIT":
		return []string{"RETRY", "STATUS", "MANUAL"}
	default:
		return []string{"STATUS"}
	}
}
func (h *Handler) noticeText(ctx context.Context, n storage.AuthRecoveryNotice, e storage.AuthRecoveryEpisode) (string, error) {
	var text string
	switch n.Kind {
	case "DETECTED":
		text = "ACB mất phiên. Đang tự đăng nhập lại. Bạn chỉ cần phản hồi khi bot yêu cầu CAPTCHA hoặc OTP."
	case "AI_READING":
		text = "Đang đọc CAPTCHA tự động."
	case "CATCHING_UP", "VERIFYING":
		text = "Đăng nhập đã được xác minh. Đang bù giao dịch; chưa khôi phục theo dõi realtime."
		if n.Kind == "VERIFYING" {
			text = "Đang xác minh phiên đăng nhập; chưa khôi phục theo dõi realtime."
		}
	case "COMPLETED":
		state, err := h.Store.TelegramAuthState(ctx, h.Client.Readiness().BotID)
		if err != nil {
			return "", err
		}
		settings, err := h.Store.GetMonitorSettings(ctx)
		if err != nil {
			return "", err
		}
		if state.Paused || !settings.Enabled || storage.ResolveSchedule(time.Now(), &settings).Mode == storage.ModePaused {
			text = "Phiên đã sẵn sàng; lịch theo dõi hiện đang tạm dừng."
		} else {
			text = "Đã khôi phục ACB. Đã bù giao dịch và tiếp tục theo dõi."
		}
	default:
		text = episodeLabel(n.Kind) + ". " + reasonLabel(e.ReasonCode) + "."
	}
	return fmt.Sprintf("%s\nKết nối %s; đợt %s; số lần thử %d.", text, shortID(e.ConnectionID), shortID(e.ID), e.AttemptCount), nil
}
