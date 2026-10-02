package telegramauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/challenge"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

type ReplyBroker interface {
	HandleReply(ctx context.Context, chatID, promptID, incomingID int64, text string) error
}
type HandlerOptions struct {
	Store           *storage.Store
	Client          *Client
	ChatID, UserID  int64
	PublicOrigin    string
	ReplyBroker     ReplyBroker
	CoordinatorWake func()
	AIDegraded      func() string
}
type PendingDeliveryBroker interface {
	DeliverPending(context.Context) error
}
type CaptchaImageBroker interface {
	RequestCaptchaImage(context.Context, storage.TelegramAuthAction) error
}

type Handler struct {
	HandlerOptions
	wake                          chan struct{}
	deliveryMu                    sync.Mutex
	deliveryRunning               bool
	lastProgress                  time.Time
	progressEpisode, progressText string
	deliveryRetryAt               time.Time
}

func NewHandler(options HandlerOptions) (*Handler, error) {
	if options.Store == nil || options.Client == nil || options.ChatID == 0 || options.UserID == 0 {
		return nil, errors.New("TELEGRAM_HANDLER_CONFIG_INVALID")
	}
	u, err := url.Parse(options.PublicOrigin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("TELEGRAM_PUBLIC_ORIGIN_INVALID")
	}
	options.PublicOrigin = strings.TrimRight(options.PublicOrigin, "/")
	return &Handler{HandlerOptions: options, wake: make(chan struct{}, 1)}, nil
}
func (h *Handler) authorized(chat Chat, from *User) bool {
	return chat.Type == "private" && chat.ID == h.ChatID && from != nil && from.ID == h.UserID && !from.IsBot
}
func present(raw []byte) bool { return len(raw) != 0 && string(raw) != "null" }
func unsafeMessage(m *Message) bool {
	return m.EditDate != 0 || m.ForwardDate != 0 || present(m.ForwardOrigin) || present(m.ForwardFrom) || present(m.ForwardFromChat) || present(m.ViaBot) || present(m.SenderChat) || present(m.Photo) || present(m.Video) || present(m.Animation) || present(m.Audio) || present(m.Voice) || present(m.VideoNote) || present(m.Document) || present(m.Contact) || present(m.Sticker) || present(m.Location) || present(m.Venue) || present(m.Poll) || present(m.Dice) || present(m.Story) || present(m.PaidMedia) || present(m.Game) || present(m.Invoice) || present(m.SuccessfulPayment) || present(m.PassportData) || present(m.WebAppData)
}
func rejectedDisposition(err error) bool {
	if errors.Is(err, storage.ErrRecoveryConsentRequired) {
		return true
	}
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
			h.wakeCoordinator()
			return nil
		}
		return err
	}
	switch m.Text {
	case "/start", "/menu", "/help":
		return h.menu(ctx, m.Text == "/help")
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
	default:
		return h.menu(ctx, true)
	}
}
func (h *Handler) say(ctx context.Context, text string) error {
	_, err := h.Client.SendText(ctx, h.ChatID, text, nil)
	return err
}
func (h *Handler) snapshot(ctx context.Context) (storage.Connection, storage.AuthRecoveryEpisode, error) {
	c, err := h.Store.Connection(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Connection{State: "UNCONFIGURED"}, storage.AuthRecoveryEpisode{}, nil
	}
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
		return "Đăng nhập"
	case "RETRY":
		return "Thử lại"
	case "CANCEL":
		return "Hủy đăng nhập"
	case "LOGOUT":
		return "Xác nhận đăng xuất ACB"
	case "UPDATE_CREDENTIALS":
		return "Cấp link đổi thông tin"
	case "PAUSE":
		return "Khóa đăng nhập"
	case "RESUME":
		return "Mở khóa đăng nhập"
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
			_ = h.Client.DeleteMessage(ctx, h.ChatID, id)
			return id, err
		}
	}
	return id, nil
}

func (h *Handler) Wake() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Handler) wakeCoordinator() {
	if h.CoordinatorWake != nil {
		h.CoordinatorWake()
	}
	h.Wake()
}

func (h *Handler) menu(ctx context.Context, help bool) error {
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	text := "Quản lý phiên ACB. Đăng nhập chỉ bắt đầu sau khi bạn bấm nút Đăng nhập."
	if help {
		text += "\nBot xử lý các bước được ACB hỗ trợ; chỉ trả lời trực tiếp yêu cầu OTP đăng nhập từ app ACB, không nhập OTP chuyển tiền. Telegram không mã hóa đầu cuối; xóa tin chỉ là cố gắng tốt nhất. Đổi thông tin chỉ thay bản lưu hệ thống, không đổi mật khẩu tại ngân hàng."
	}
	return h.sendPanel(ctx, c, e, text)
}

func (h *Handler) sendPanel(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, text string) error {
	return h.sendNoticePanel(ctx, c, e, text, nil)
}

func (h *Handler) sendNoticePanel(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, text string, deliveredID *int64) error {
	keyboard := inlineKeyboard{Rows: [][]inlineButton{{{Text: "Trạng thái", Data: "nav:status"}}}}
	var actions []storage.TelegramAuthAction
	locked := false
	if err := h.Store.CheckMutationAllowed(ctx); err != nil {
		if !errors.Is(err, storage.ErrMutationGateLocked) {
			return err
		}
		locked = true
		text += "\nHệ thống đang bảo trì; thao tác thay đổi tạm khóa. Trạng thái và trợ giúp vẫn hoạt động."
	}
	configured := false
	if c.ID != "" && c.State != "UNCONFIGURED" {
		var err error
		configured, err = h.Store.HasACBCredentials(ctx, c.ID)
		if err != nil {
			return err
		}
	}
	if !configured {
		text += "\nChưa khởi tạo — chạy setup/import trên VPS. Chưa đăng nhập ACB."
	}
	bot, err := h.Store.ReadTelegramAuthState(ctx, h.Client.Readiness().BotID)
	if err != nil {
		return err
	}
	active := false
	if e.AttemptID != "" {
		report, err := h.Store.ActiveAuthAttempts(ctx)
		if err != nil {
			return err
		}
		for _, attempt := range report.Attempts {
			if attempt.ID == e.AttemptID {
				active = true
			}
		}
	}
	login := inlineButton{Text: "Đăng nhập", Data: "nav:menu"}
	operations := []string{}
	if !locked && configured && !bot.Paused && !active && c.State != "MONITORING" {
		operations = append(operations, "LOGIN")
	}
	if !locked && c.ID != "" {
		if bot.Paused {
			operations = append(operations, "RESUME")
		} else {
			operations = append(operations, "PAUSE")
		}
		if active {
			operations = append(operations, "CANCEL")
		}
		if e.RecoveryRunID != "" && c.State == "MONITORING" {
			run, err := h.Store.GetRecoveryRun(ctx, e.RecoveryRunID)
			if err != nil {
				return err
			}
			if run.Status == storage.RecoveryRunStatusFailed || run.Status == storage.RecoveryRunStatusCanceled {
				operations = append(operations, "RETRY")
			}
		}
	}
	for _, operation := range operations {
		buttons, created, err := h.buttons(ctx, c, e, []string{operation})
		if errors.Is(err, storage.ErrMutationGateLocked) {
			return h.sendNoticePanel(ctx, c, e, text, deliveredID)
		}
		if err != nil {
			return err
		}
		actions = append(actions, created...)
		button := buttons.Rows[0][0]
		if operation == "LOGIN" {
			login = button
		} else {
			if operation == "RETRY" {
				button.Text = "Thử lại đồng bộ"
			}
			keyboard.Rows = append(keyboard.Rows, []inlineButton{button})
		}
	}
	if !locked && active && e.AttemptID != "" {
		ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
		if err == nil && ch.Kind == "CAPTCHA_TEXT" && (ch.Status == "PENDING" || ch.Status == "DELIVERING") {
			expires, parseErr := time.Parse(time.RFC3339Nano, ch.ExpiresAt)
			if parseErr == nil && time.Now().Before(expires) {
				a, err := h.Store.CreateTelegramAuthAction(ctx, storage.TelegramAuthAction{BotID: h.Client.Readiness().BotID, ChatID: h.ChatID, UserID: h.UserID, EpisodeID: e.ID, ExpectedGeneration: e.Generation, AttemptID: e.AttemptID, BrowserRevision: ch.BrowserRevision, Action: "CAPTCHA_IMAGE"})
				if err != nil {
					return err
				}
				actions = append(actions, a)
				keyboard.Rows = append(keyboard.Rows, []inlineButton{{Text: "Xem ảnh captcha", Data: "ar:" + a.ID}})
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	keyboard.Rows = append(keyboard.Rows,
		[]inlineButton{login},
		[]inlineButton{{Text: "Đổi thông tin đăng nhập", Data: "nav:credentials"}},
		[]inlineButton{{Text: "Đăng xuất ACB", Data: "nav:logout"}},
		[]inlineButton{{Text: "Trợ giúp", Data: "nav:help"}, {Text: "Menu", Data: "nav:menu"}},
	)
	id, err := h.Client.SendText(ctx, h.ChatID, text, keyboard)
	if err != nil {
		return err
	}
	for _, action := range actions {
		if err := h.Store.DeliverTelegramAuthAction(ctx, action.ID, id); err != nil {
			_ = h.Client.DeleteMessage(ctx, h.ChatID, id)
			return err
		}
	}
	if deliveredID != nil {
		*deliveredID = id
	}
	return nil
}
func (h *Handler) confirm(ctx context.Context, operation string) error {
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	if c.ID == "" || c.State == "UNCONFIGURED" {
		return h.menu(ctx, false)
	}
	if err := h.Store.CheckMutationAllowed(ctx); err != nil {
		if errors.Is(err, storage.ErrMutationGateLocked) {
			return h.menu(ctx, false)
		}
		return err
	}
	if operation == "LOGIN" || operation == "UPDATE_CREDENTIALS" {
		configured, err := h.Store.HasACBCredentials(ctx, c.ID)
		if err != nil {
			return err
		}
		if !configured {
			return h.menu(ctx, false)
		}
	}
	if operation == "LOGIN" || operation == "RETRY" && c.State != "MONITORING" {
		state, err := h.Store.ReadTelegramAuthState(ctx, h.Client.Readiness().BotID)
		if err != nil {
			return err
		}
		if state.Paused {
			return h.menu(ctx, false)
		}
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
	text := fmt.Sprintf("%s? Nút dùng một lần, hết hạn sau 60 giây.", actionLabel(operation))
	if operation == "UPDATE_CREDENTIALS" {
		text += " Link HTTPS có hạn 5 phút và yêu cầu đăng nhập chủ tài khoản. Chỉ thay thông tin lưu trên VPS, không đổi mật khẩu ngân hàng và không tự đăng nhập."
	}
	if operation == "LOGOUT" {
		text += " Hệ thống sẽ xóa phiên trên VPS và thử thu hồi phiên tại ACB. Nếu ngân hàng không xác nhận, bot sẽ báo rõ; thông tin đăng nhập và giao dịch vẫn được giữ."
	}
	_, err = h.sendButtons(ctx, c, e, text, []string{operation})
	return err
}
func (h *Handler) direct(ctx context.Context, operation string) error {
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	disposition, err := h.Store.ApplyTelegramAuthOperation(ctx, h.Client.Readiness().BotID, e.ID, c.Generation, operation)
	if err != nil {
		if rejectedDisposition(err) || errors.Is(err, storage.ErrMutationGateLocked) {
			return h.say(ctx, operationFailure(err))
		}
		return err
	}
	return h.afterOperation(ctx, disposition)
}
func (h *Handler) handleCallback(ctx context.Context, q *CallbackQuery) error {
	if q.Message == nil || q.InlineMessageID != "" || !h.authorized(q.Message.Chat, &q.From) || unsafeMessage(q.Message) {
		return nil
	}
	// Clear the spinner before storage or delivery work, with its own deadline.
	ackCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_ = h.Client.AnswerCallbackQuery(ackCtx, q.ID)
	cancel()
	switch q.Data {
	case "nav:menu":
		return h.menu(ctx, false)
	case "nav:status":
		return h.status(ctx)
	case "nav:help":
		return h.menu(ctx, true)
	case "nav:credentials":
		return h.confirm(ctx, "UPDATE_CREDENTIALS")
	case "nav:logout":
		return h.confirm(ctx, "LOGOUT")
	}
	if !strings.HasPrefix(q.Data, "ar:") || len(q.Data) != 25 {
		return h.menu(ctx, false)
	}
	a, disposition, err := h.Store.ConsumeTelegramAuthAction(ctx, strings.TrimPrefix(q.Data, "ar:"), h.Client.Readiness().BotID, h.ChatID, h.UserID, q.Message.ID, time.Now())
	if err != nil {
		if errors.Is(err, storage.ErrChallengeExpired) {
			return h.menu(ctx, false)
		}
		if rejectedDisposition(err) || errors.Is(err, storage.ErrMutationGateLocked) {
			return h.say(ctx, operationFailure(err))
		}
		return err
	}
	if disposition == "CREDENTIAL_GRANT" {
		_, err := h.Client.SendText(ctx, h.ChatID, "Link đổi thông tin đăng nhập có hạn 5 phút, yêu cầu đăng nhập chủ tài khoản. Nếu trình duyệt Telegram chưa đăng nhập Cloudflare Access, hãy mở bằng trình duyệt thường. Chỉ thay bản lưu trên VPS; chưa đăng nhập ACB.\n"+h.PublicOrigin+"/admin/acb-credentials#grant="+a.CredentialGrantToken, nil)
		a.CredentialGrantToken = ""
		return err
	}
	if disposition == "CAPTCHA_IMAGE" {
		broker, ok := h.ReplyBroker.(CaptchaImageBroker)
		if !ok {
			return errors.New("TELEGRAM_CAPTCHA_BROKER_UNAVAILABLE")
		}
		if err := broker.RequestCaptchaImage(ctx, a); err != nil {
			return err
		}
		h.Wake()
		return nil
	}
	return h.afterOperation(ctx, disposition)
}
func operationFailure(err error) string {
	switch {
	case errors.Is(err, storage.ErrMutationGateLocked):
		return "Hệ thống đang bảo trì. Bạn vẫn có thể xem Trạng thái và Trợ giúp."
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
func (h *Handler) afterOperation(ctx context.Context, disposition string) error {
	h.wakeCoordinator()
	switch disposition {
	case "LOGOUT_QUEUED":
		return h.say(ctx, "Đã nhận yêu cầu đăng xuất. Phiên lưu đã bị chặn; bot đang chờ worker xóa trạng thái bộ nhớ và thử thu hồi phía ACB. Kết quả hai phía sẽ được báo riêng.")
	case "STATUS":
		return h.status(ctx)
	case "CATCHUP_RETRY":
		return h.say(ctx, "Đã yêu cầu thử lại bù giao dịch với phiên hiện tại; không đăng nhập hoặc yêu cầu OTP lại.")
	case "ACTIVE":
		return h.say(ctx, "Phiên đang hoạt động.")
	case "REARMED":
		return h.say(ctx, "Đã nhận yêu cầu đăng nhập một lần. Bot sẽ xử lý và yêu cầu OTP từ app ACB khi cần.")
	case "PAUSE":
		return h.say(ctx, "Đã khóa đăng nhập và hủy yêu cầu đang chờ. Theo dõi với phiên đã xác minh và bù giao dịch vẫn tiếp tục.")
	case "RESUMED":
		return h.say(ctx, "Đã mở khóa đăng nhập. Chưa đăng nhập ACB; hãy bấm Đăng nhập khi cần.")
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
	bot, err := h.Store.ReadTelegramAuthState(ctx, h.Client.Readiness().BotID)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("Kết nối %s: %s.\nĐợt %s: %s. Số lần thử: %d.\nKhóa đăng nhập: %t.", shortID(c.ID), connectionLabel(c.State), shortID(e.ID), episodeLabel(e.State), e.AttemptCount, bot.Paused)
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
	return h.sendPanel(ctx, c, e, text)
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
		return "Ngân hàng yêu cầu phương thức không hỗ trợ; xử lý trong ứng dụng ACB hoặc liên hệ ngân hàng"
	case "MANUAL_ACTIVE":
		return "Đang có phiên manual"
	default:
		return "Cần kiểm tra trạng thái và xác nhận bước tiếp theo"
	}
}

// RunDelivery is the sole runtime sender for outbox, challenge and progress.
// It never shares the coordinator's bank-operation mutex.
func (h *Handler) RunDelivery(ctx context.Context) error {
	h.deliveryMu.Lock()
	if h.deliveryRunning {
		h.deliveryMu.Unlock()
		return errors.New("TELEGRAM_DELIVERY_ALREADY_RUNNING")
	}
	h.deliveryRunning = true
	h.deliveryMu.Unlock()
	defer func() { h.deliveryMu.Lock(); h.deliveryRunning = false; h.deliveryMu.Unlock() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	h.Wake()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-h.wake:
		}
		if time.Now().Before(h.deliveryRetryAt) {
			continue
		}
		err := h.deliverLogoutNotices(ctx)
		if err == nil {
			err = h.DeliverNotices(ctx)
		}
		if err == nil {
			if broker, ok := h.ReplyBroker.(PendingDeliveryBroker); ok {
				err = broker.DeliverPending(ctx)
			}
		}
		if err == nil {
			err = h.deliverProgress(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var transport *TransportError
			if errors.As(err, &transport) {
				if transport.Fatal {
					return err
				}
				h.deliveryRetryAt = time.Now().Add(deliveryDelay(err))
			}
		}
	}
}

func deliveryDelay(err error) time.Duration {
	delay := 30 * time.Second
	var transport *TransportError
	if errors.As(err, &transport) && transport.RetryAfter > delay {
		delay = transport.RetryAfter
	}
	return delay
}

func progressKind(kind string) bool {
	switch kind {
	case "STARTING", "AI_READING", "LOGIN", "WAITING_CAPTCHA", "WAITING_OTP", "VERIFYING", "CATCHING_UP":
		return true
	default:
		return false
	}
}

// Delivery is at-least-once. Coalescing records obsolete steps as delivered
// without replaying their text or generating a bank operation.
func (h *Handler) DeliverNotices(ctx context.Context) error {
	notices, err := h.Store.PendingAuthRecoveryNotices(ctx)
	if err != nil {
		return err
	}
	c, current, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	for _, n := range notices {
		e, err := h.Store.AuthRecoveryEpisode(ctx, n.EpisodeID)
		if err != nil {
			return err
		}
		if e.ID != current.ID || e.Generation != c.Generation || progressKind(n.Kind) || n.Kind != e.State && n.Kind != "CREDENTIALS_UPDATED" || n.Kind != "CREDENTIALS_UPDATED" && n.EventKey != fmt.Sprintf("%s:%s:%d", e.ID, n.Kind, e.AttemptCount) {
			if err := h.Store.CoalesceAuthRecoveryNotice(ctx, n.ID); err != nil {
				return err
			}
			continue
		}
		if n.NextAttemptAt != "" {
			when, err := time.Parse(time.RFC3339Nano, n.NextAttemptAt)
			if err != nil {
				return errors.New("TELEGRAM_NOTICE_RETRY_INVALID")
			}
			if time.Now().Before(when) {
				continue
			}
		}
		text, err := h.noticeText(ctx, n, e)
		if err != nil {
			return err
		}
		// Actionable notices use the same fence-aware operator menu. No nonce
		// is created while maintenance locks mutations.
		beforeID := int64(0)
		err = h.sendNoticePanel(ctx, c, e, text, &beforeID)
		if err != nil {
			if persistErr := h.Store.FinishAuthRecoveryNotice(ctx, n.ID, 0, time.Now().Add(deliveryDelay(err))); persistErr != nil {
				return persistErr
			}
			return err
		}
		if err := h.Store.FinishAuthRecoveryNotice(ctx, n.ID, beforeID, time.Time{}); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) deliverProgress(ctx context.Context) error {
	if time.Since(h.lastProgress) < time.Second {
		return nil
	}
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	if e.ID == "" {
		return nil
	}
	text := ""
	switch e.State {
	case "STARTING":
		text = "Đang mở ACB"
	case "LOGIN":
		text = "Đang xử lý bước đăng nhập ACB"
		loginAt, loginErr := time.Parse(time.RFC3339Nano, e.LastLoginAt)
		consentAt, consentErr := time.Parse(time.RFC3339Nano, e.ConsentConsumedAt)
		if loginErr == nil && consentErr == nil && !loginAt.Before(consentAt) {
			text = "Đang gửi thông tin đăng nhập"
		} else if e.AIUsed > 0 {
			text = "Đang đọc captcha bằng AI"
		}
	case "WAITING_CAPTCHA":
		text = "Chờ CAPTCHA"
	case "WAITING_OTP":
		text = "Chờ OTP đăng nhập từ app ACB"
		ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
		if err == nil {
			text += " (hạn " + localExpiry(ch.ExpiresAt) + ")"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	case "VERIFYING":
		text = "Đang xác minh phiên"
	case "CATCHING_UP":
		text = "Đang bù giao dịch từ " + e.RequiredFrom + " đến " + e.RequiredTo
	case "COMPLETED":
		text = "Đã sẵn sàng"
	default:
		return nil
	}
	if h.progressEpisode == e.ID && h.progressText == text {
		return nil
	}
	markup := inlineKeyboard{Rows: [][]inlineButton{{{Text: "Trạng thái", Data: "nav:status"}, {Text: "Menu", Data: "nav:menu"}}}}
	h.lastProgress = time.Now()
	if e.StatusMessageID > 0 {
		err = h.Client.EditText(ctx, h.ChatID, e.StatusMessageID, text, markup)
		if err != nil {
			var transport *TransportError
			if !errors.As(err, &transport) || transport.Code != "TELEGRAM_MESSAGE_UNEDITABLE" {
				return err
			}
		} else {
			h.progressEpisode, h.progressText = e.ID, text
			return nil
		}
	}
	id, err := h.Client.SendText(ctx, h.ChatID, text, markup)
	if err != nil {
		return err
	}
	if err := h.Store.SetAuthRecoveryStatusMessage(ctx, e.ID, c.Generation, id); err != nil {
		_ = h.Client.DeleteMessage(ctx, h.ChatID, id)
		return err
	}
	h.progressEpisode, h.progressText = e.ID, text
	return nil
}
func (h *Handler) noticeText(ctx context.Context, n storage.AuthRecoveryNotice, e storage.AuthRecoveryEpisode) (string, error) {
	var text string
	switch n.Kind {
	case "DETECTED":
		text = "ACB mất phiên. Hệ thống chưa đăng nhập lại. Bấm Đăng nhập để bot tự xử lý; bạn chỉ cần lấy OTP trong app ACB khi được yêu cầu."
	case "CREDENTIALS_UPDATED":
		text = "Đã lưu thông tin đăng nhập. Chưa đăng nhập ACB. Bấm Đăng nhập khi bạn sẵn sàng."
	case "AI_READING":
		text = "Đang đọc CAPTCHA tự động."
	case "CATCHING_UP", "VERIFYING":
		text = "Đăng nhập đã được xác minh. Đang bù giao dịch; chưa khôi phục theo dõi realtime."
		if n.Kind == "VERIFYING" {
			text = "Đang xác minh phiên đăng nhập; chưa khôi phục theo dõi realtime."
		}
	case "COMPLETED":
		state, err := h.Store.ReadTelegramAuthState(ctx, h.Client.Readiness().BotID)
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
func (h *Handler) deliverLogoutNotices(ctx context.Context) error {
	jobs, err := h.Store.PendingACBLogoutNotices(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		local := "Đang chờ worker xóa trạng thái bộ nhớ; chưa xác nhận xóa phiên local hoàn tất."
		if job.LocalClearedAt != "" {
			local = "Đã xóa phiên lưu và trạng thái bộ nhớ của hệ thống trên VPS."
		}
		bank := "Chưa có kết quả thu hồi phiên phía ACB."
		switch job.BankStatus {
		case "CONFIRMED":
			bank = "ACB đã xác nhận phiên trước bị thu hồi."
		case "ALREADY_EXPIRED":
			bank = "Phiên trước đã hết hạn tại ACB trước thao tác đăng xuất."
		case "UNCONFIRMED":
			bank = "Chưa xác nhận ACB thu hồi phiên. Nếu cần, kiểm tra hoặc đăng xuất trong app ACB."
		case "IN_FLIGHT":
			bank = "Đang thử thu hồi phiên phía ACB; chưa xác nhận thành công."
		}
		text := local + "\n" + bank + "\nHệ thống chưa đăng nhập lại. Mở khóa đăng nhập không tạo phiên mới."
		messageID, err := h.Client.SendText(ctx, h.ChatID, text, inlineKeyboard{Rows: [][]inlineButton{{{Text: "Trạng thái", Data: "nav:status"}, {Text: "Menu", Data: "nav:menu"}}}})
		if err != nil {
			return err
		}
		if err := h.Store.FinishACBLogoutNotice(ctx, job.ID, messageID, job.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}
