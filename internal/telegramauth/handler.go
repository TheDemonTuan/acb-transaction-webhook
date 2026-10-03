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
	progressMu                    sync.Mutex
	progressEpisode, progressText string
	progressKey                   string
	progressMarkup                inlineKeyboard
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
		if errors.Is(err, challenge.ErrInvalidResponse) {
			return nil // The broker sends one kind-specific format correction.
		}
		if err == nil {
			h.wakeCoordinator()
			return h.refreshPanel(ctx, "Đã gửi câu trả lời tới ACB. Chỉ kết quả xác minh bên dưới mới xác nhận đăng nhập thành công; không gửi lại mã.")
		}
		if errors.Is(err, challenge.ErrOutcomeUnknown) {
			h.wakeCoordinator()
			return h.refreshPanel(ctx, "Chưa xác định được ACB có nhận mã hay không. Bot không gửi lại mã. Chờ kết quả dừng/xác minh; kiểm tra app ACB trước khi bắt đầu lần mới.")
		}
		if rejectedDisposition(err) {
			h.wakeCoordinator()
			text := "Yêu cầu này không còn dùng được. Không gửi mã cũ; làm theo yêu cầu mới nhất còn hạn hoặc nút bên dưới."
			if errors.Is(err, storage.ErrChallengeExpired) {
				text = "Mã đã hết thời gian trả lời nên chưa được gửi tới ACB. Không gửi mã cũ; chờ kết quả dừng và dùng Đăng nhập cho lần mới."
			} else if errors.Is(err, storage.ErrChallengeConsumed) {
				text = "Câu trả lời cho yêu cầu này đã được xử lý hoặc đang chờ kết quả. Bot không gửi lại mã; hãy theo dõi tiến độ bên dưới."
			}
			return h.refreshPanel(ctx, text)
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
		return "Lưu lại thông tin đăng nhập"
	case "PAUSE":
		return "Tạm khóa đăng nhập"
	case "RESUME":
		return "Cho phép đăng nhập"
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
	text, err := h.stateText(ctx, c, e, false)
	if err != nil {
		return err
	}
	if help {
		text += "\n\nMỗi lần bấm Đăng nhập chỉ cho phép một lần thử. Dừng hoặc khởi động lại không tự đăng nhập. Chỉ trả lời trực tiếp tin/ảnh yêu cầu OTP đăng nhập hoặc captcha còn hạn; không gửi mật khẩu hay OTP chuyển tiền vào chat. Telegram không mã hóa đầu cuối; xóa tin chỉ là cố gắng tốt nhất. Đổi thông tin chỉ thay bản lưu hệ thống, không đổi mật khẩu tại ngân hàng."
	}
	return h.sendPanel(ctx, c, e, text)
}

func (h *Handler) sendPanel(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, text string) error {
	return h.sendNoticePanel(ctx, c, e, text, nil)
}

type panelPlan struct {
	text       string
	keyboard   inlineKeyboard
	operations []string
	captcha    *storage.AuthChallenge
}

func (h *Handler) planPanel(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, text string) (panelPlan, error) {
	p := panelPlan{text: text}
	locked := false
	if err := h.Store.CheckMutationAllowed(ctx); err != nil {
		if !errors.Is(err, storage.ErrMutationGateLocked) {
			return p, err
		}
		locked = true
		p.text += "\nHệ thống đang bảo trì; chỉ xem trạng thái và trợ giúp, chưa thể thay đổi."
	}
	configured := false
	if c.ID != "" {
		var err error
		configured, err = h.Store.HasACBCredentials(ctx, c.ID)
		if err != nil {
			return p, err
		}
	}
	bot, err := h.Store.ReadTelegramAuthState(ctx, h.Client.Readiness().BotID)
	if err != nil {
		return p, err
	}
	report, err := h.Store.ActiveAuthAttempts(ctx)
	if err != nil {
		return p, err
	}
	active, otherActive := false, false
	for _, attempt := range report.Attempts {
		if attempt.ID == e.AttemptID && attempt.OwnerSubject == storage.RecoveryOwner {
			active = true
		} else {
			otherActive = true
		}
	}
	if !configured {
		p.text += "\nChưa lưu thông tin đăng nhập. Nhờ người quản lý cấu hình hệ thống trước; không gửi mật khẩu vào chat."
	}
	if bot.Paused {
		p.text += "\nĐăng nhập đang tạm khóa. Cho phép lại không tự đăng nhập."
	}
	if otherActive {
		p.text += "\nNgười quản lý đang đăng nhập ở nơi khác. Bot chờ và không hủy lần đó."
	}
	if !locked && c.ID != "" {
		if (active && c.State != "MONITORING") || pendingConsent(e) {
			p.operations = append(p.operations, "CANCEL")
		} else if !otherActive && !bot.Paused && configured && (c.State == "AUTH_REQUIRED" || c.State == "UNCONFIGURED") && e.RecoveryRunID == "" {
			readyAt, _ := time.Parse(time.RFC3339Nano, e.NextAttemptAt)
			loginAt, parseErr := time.Parse(time.RFC3339Nano, e.LastLoginAt)
			if parseErr == nil && loginAt.Add(time.Minute).After(readyAt) {
				readyAt = loginAt.Add(time.Minute)
			}
			if time.Now().Before(readyAt) {
				p.text += "\nCó thể thử lại sau " + localExpiry(readyAt.Format(time.RFC3339Nano)) + ". Bấm Làm mới khi đến giờ; bot không tự thử."
			} else {
				p.operations = append(p.operations, "LOGIN")
			}
		}
		if e.RecoveryRunID != "" && c.State == "MONITORING" && e.ReasonCode != "INVALID_CHECKPOINT" {
			run, err := h.Store.GetRecoveryRun(ctx, e.RecoveryRunID)
			if err != nil {
				return p, err
			}
			if (run.Status == storage.RecoveryRunStatusFailed || run.Status == storage.RecoveryRunStatusCanceled) && run.ErrorCode != "INVALID_CHECKPOINT" {
				p.operations = append(p.operations, "RETRY")
			}
		}
		if active && e.AttemptID != "" {
			ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
			if err == nil && ch.Kind == "CAPTCHA_TEXT" && (ch.Status == "PENDING" || ch.Status == "DELIVERING") {
				expires, parseErr := time.Parse(time.RFC3339Nano, ch.ExpiresAt)
				if parseErr == nil && time.Now().Before(expires) {
					p.captcha = &ch
				}
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return p, err
			}
		}
		if !active && configured {
			p.keyboard.Rows = append(p.keyboard.Rows, []inlineButton{{Text: "Thông tin đăng nhập", Data: "nav:credentials"}})
		}
		if bot.Paused {
			p.operations = append(p.operations, "RESUME")
		} else if !active {
			p.operations = append(p.operations, "PAUSE")
		}
		if c.State == "MONITORING" && !active {
			p.keyboard.Rows = append(p.keyboard.Rows, []inlineButton{{Text: "Đăng xuất ACB", Data: "nav:logout"}})
		}
	}
	p.keyboard.Rows = append(p.keyboard.Rows, []inlineButton{{Text: "Làm mới", Data: "nav:status"}, {Text: "Hướng dẫn", Data: "nav:help"}})
	return p, nil
}

func (h *Handler) renderPanel(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, p panelPlan) (inlineKeyboard, []storage.TelegramAuthAction, error) {
	keyboard, actions, err := h.buttons(ctx, c, e, p.operations)
	if err != nil {
		return keyboard, actions, err
	}
	for i, operation := range p.operations {
		if operation == "RETRY" {
			keyboard.Rows[i][0].Text = "Thử lại lấy giao dịch thiếu"
		}
	}
	if p.captcha != nil {
		a, err := h.Store.CreateTelegramAuthAction(ctx, storage.TelegramAuthAction{BotID: h.Client.Readiness().BotID, ChatID: h.ChatID, UserID: h.UserID, EpisodeID: e.ID, ExpectedGeneration: c.Generation, AttemptID: e.AttemptID, BrowserRevision: p.captcha.BrowserRevision, Action: "CAPTCHA_IMAGE"})
		if err != nil {
			return keyboard, actions, err
		}
		actions = append(actions, a)
		keyboard.Rows = append(keyboard.Rows, []inlineButton{{Text: "Xem ảnh captcha", Data: "ar:" + a.ID}})
	}
	keyboard.Rows = append(keyboard.Rows, p.keyboard.Rows...)
	return keyboard, actions, nil
}

func (h *Handler) bindActions(ctx context.Context, actions []storage.TelegramAuthAction, id int64) error {
	for _, action := range actions {
		if err := h.Store.DeliverTelegramAuthAction(ctx, action.ID, id); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) sendNoticePanel(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, text string, deliveredID *int64) error {
	p, err := h.planPanel(ctx, c, e, text)
	if err != nil {
		return err
	}
	keyboard, actions, err := h.renderPanel(ctx, c, e, p)
	if errors.Is(err, storage.ErrMutationGateLocked) {
		return h.sendNoticePanel(ctx, c, e, text, deliveredID)
	}
	if err != nil {
		return err
	}
	id, err := h.Client.SendText(ctx, h.ChatID, p.text, keyboard)
	if err != nil {
		return err
	}
	if err := h.bindActions(ctx, actions, id); err != nil {
		_ = h.Client.DeleteMessage(ctx, h.ChatID, id)
		return err
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
			return h.status(ctx)
		}
		return h.refreshPanel(ctx, "Phiên đã xác minh; đang lấy giao dịch còn thiếu. Chỉ thử lại bước lấy giao dịch nếu đã thất bại; không đăng nhập lại.")
	}
	if (operation == "CANCEL") && e.RecoveryRunID != "" && c.State == "MONITORING" {
		return h.refreshPanel(ctx, "Không thể hủy bước lấy giao dịch đã bắt đầu. Phiên đã xác minh được giữ; tạm khóa đăng nhập chỉ ngăn lần đăng nhập tiếp.")
	}
	if operation == "LOGIN" || operation == "RETRY" {
		report, err := h.Store.ActiveAuthAttempts(ctx)
		if err != nil {
			return err
		}
		for _, a := range report.Attempts {
			if a.OwnerSubject != storage.RecoveryOwner {
				return h.refreshPanel(ctx, "Người quản lý đang đăng nhập ở nơi khác. Bot chờ và không hủy lần đó.")
			}
		}
		if e.AttemptID != "" {
			ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
			if err == nil {
				expiry, parseErr := time.Parse(time.RFC3339Nano, ch.ExpiresAt)
				if parseErr == nil && time.Now().Before(expiry) {
					return h.refreshPanel(ctx, "Đang chờ captcha hoặc OTP còn hạn. Trả lời trực tiếp yêu cầu đó; không tạo thêm lần đăng nhập.")
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
			return h.refreshPanel(ctx, operationFailure(err))
		}
		return err
	}
	return h.afterOperation(ctx, disposition)
}
func (h *Handler) handleCallback(ctx context.Context, q *CallbackQuery) error {
	if q.Message == nil || q.InlineMessageID != "" || !h.authorized(q.Message.Chat, &q.From) {
		return nil
	}
	message := *q.Message
	// Telegram includes edit_date when a bot's persistent progress is updated.
	// Only our own bot messages may use that exception; action binding still
	// fences the displayed message ID, operator and connection generation.
	if message.From != nil && message.From.IsBot && message.From.ID == h.Client.Readiness().BotID {
		message.EditDate = 0
	}
	if unsafeMessage(&message) {
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
			return h.refreshPanel(ctx, "Nút đã hết hạn. Dùng nút mới bên dưới; chưa bắt đầu thêm lần đăng nhập.")
		}
		if rejectedDisposition(err) || errors.Is(err, storage.ErrMutationGateLocked) {
			return h.refreshPanel(ctx, operationFailure(err))
		}
		return err
	}
	if disposition == "CREDENTIAL_GRANT" {
		_, err := h.Client.SendText(ctx, h.ChatID, "Đã cấp link lưu thông tin đăng nhập, có hạn 5 phút.\n1. Mở link và đăng nhập chủ tài khoản để lưu thông tin. Nếu trình duyệt Telegram chưa đăng nhập, mở bằng trình duyệt thường.\n2. Sau khi lưu, bot sẽ báo kết quả; bấm Đăng nhập khi bạn sẵn sàng. Chưa đăng nhập ACB, không đổi mật khẩu ngân hàng.\n"+h.PublicOrigin+"/admin/acb-credentials#grant="+a.CredentialGrantToken, inlineKeyboard{Rows: [][]inlineButton{{{Text: "Quay lại ACB", Data: "nav:menu"}}}})
		a.CredentialGrantToken = ""
		return err
	}
	if disposition == "CAPTCHA_IMAGE" {
		broker, ok := h.ReplyBroker.(CaptchaImageBroker)
		if !ok {
			return errors.New("TELEGRAM_CAPTCHA_BROKER_UNAVAILABLE")
		}
		if err := broker.RequestCaptchaImage(ctx, a); err != nil {
			if rejectedDisposition(err) {
				return h.refreshPanel(ctx, "Ảnh captcha cũ không còn dùng được. Hãy làm theo yêu cầu mới nhất còn hạn.")
			}
			return err
		}
		h.Wake()
		return h.say(ctx, "Đã nhận yêu cầu xem thêm ảnh captcha. Đang lấy ảnh hiện tại; ảnh xem thêm không tạo mã mới. Để gửi ký tự, trả lời ảnh yêu cầu gốc còn hạn.")
	}
	return h.afterOperation(ctx, disposition)
}
func operationFailure(err error) string {
	switch {
	case errors.Is(err, storage.ErrMutationGateLocked):
		return "Hệ thống đang bảo trì. Bạn vẫn có thể xem Trạng thái và Trợ giúp."
	case errors.Is(err, storage.ErrRecoveryCommitted):
		return "Không thể bỏ qua lỗi dữ liệu giao dịch bằng hủy hoặc đăng nhập lại. Nhờ người quản lý sửa mốc lấy giao dịch; phiên hiện tại vẫn được giữ."
	case errors.Is(err, storage.ErrRecoveryCooldown):
		return "Chưa đủ 60 giây từ lần gửi đăng nhập trước. Hãy chờ rồi xác nhận lại."
	case errors.Is(err, storage.ErrAuthAttemptActive):
		return "Đang có lần đăng nhập chưa kết thúc. Hãy chờ tiến độ hoặc trả lời yêu cầu mã còn hạn; bot không tạo thêm lần đăng nhập."
	default:
		return "Nút đã dùng hoặc trạng thái vừa thay đổi. Dùng nút mới bên dưới; bot chưa tạo thêm lần đăng nhập."
	}
}
func (h *Handler) refreshPanel(ctx context.Context, result string) error {
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	text, err := h.stateText(ctx, c, e, false)
	if err != nil {
		return err
	}
	return h.sendPanel(ctx, c, e, result+"\n\n"+text)
}

func (h *Handler) afterOperation(ctx context.Context, disposition string) error {
	h.wakeCoordinator()
	switch disposition {
	case "LOGOUT_QUEUED":
		return h.refreshPanel(ctx, "Đã nhận yêu cầu đăng xuất.\n1/2 · Đang xóa phiên lưu trên hệ thống.\n2/2 · Sẽ thử thu hồi phiên tại ACB và báo kết quả riêng. Chưa xác nhận đăng xuất ngân hàng hoàn tất; không tự đăng nhập lại.")
	case "STATUS", "ACTIVE":
		return h.status(ctx)
	case "CATCHUP_RETRY", "REARMED":
		return h.deliverProgress(ctx)
	case "PAUSE":
		return h.refreshPanel(ctx, "Đã tạm khóa đăng nhập và hủy lần đang chờ. Không tự đăng nhập lại. Nếu phiên đã xác minh, theo dõi và lấy giao dịch còn thiếu vẫn tiếp tục theo lịch.")
	case "RESUMED":
		return h.refreshPanel(ctx, "Đã cho phép đăng nhập. Thao tác này không bắt đầu lần mới; bấm Đăng nhập khi cần.")
	case "CANCEL":
		if err := h.deliverProgress(ctx); err != nil {
			return err
		}
		c, e, err := h.snapshot(ctx)
		if err != nil {
			return err
		}
		if e.StatusMessageID > 0 {
			return nil
		}
		return h.sendPanel(ctx, c, e, "Đã hủy lần này. Không tự đăng nhập lại; bấm Đăng nhập khi muốn bắt đầu lần mới.")
	default:
		return errors.New("TELEGRAM_OPERATION_INVALID")
	}
}
func (h *Handler) status(ctx context.Context) error {
	return h.menu(ctx, false)
}

func (h *Handler) stateText(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, timed bool) (string, error) {
	text := "ACB: " + connectionLabel(c.State) + "."
	switch e.State {
	case "DETECTED":
		if pendingConsent(e) {
			text = "Đã nhận yêu cầu đăng nhập một lần. Đang chờ mở ACB; bạn chưa cần gửi mã."
		} else if e.ConsentActionID != "" && e.ConsentConsumedAt == "" {
			text += "\nYêu cầu đăng nhập đã hết hạn trước khi mở ACB. Bấm Đăng nhập nếu muốn xác nhận lần mới; bot không tự thử."
		} else {
			text += "\nChưa đăng nhập lại. Bấm Đăng nhập khi bạn sẵn sàng nhận OTP."
		}
	case "STARTING":
		text = "1/6 · Đang mở trang ACB.\nBạn chờ ở đây; bot sẽ cập nhật trong tin này."
	case "LOGIN":
		text = "1/6 · Đang chờ trang đăng nhập ACB sẵn sàng.\nBạn chưa cần gửi mã."
		loginAt, loginErr := time.Parse(time.RFC3339Nano, e.LastLoginAt)
		consentAt, consentErr := time.Parse(time.RFC3339Nano, e.ConsentConsumedAt)
		if loginErr == nil && consentErr == nil && !loginAt.Before(consentAt) {
			text = "3/6 · Đang gửi thông tin đăng nhập và chờ ACB phản hồi.\nChưa xác nhận đăng nhập thành công; không gửi lại mật khẩu hoặc mã."
		} else if e.AIUsed > 0 {
			text = "2/6 · Đang đọc captcha tự động.\nBạn chờ; nếu cần nhập tay, bot sẽ gửi ảnh riêng để trả lời."
		}
	case "WAITING_CAPTCHA", "WAITING_OTP":
		if e.State == "WAITING_OTP" {
			text = "4/6 · Chờ OTP đăng nhập.\nMở app ACB lấy mã cho lần đăng nhập này, rồi trả lời trực tiếp tin yêu cầu OTP riêng (không trả lời tin tiến độ này). Không dùng OTP chuyển tiền."
		} else {
			text = "2/6 · Chờ bạn nhập captcha.\nTrả lời trực tiếp ảnh yêu cầu captcha bằng ký tự trong ảnh (không trả lời tin tiến độ này)."
		}
		ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
		if err == nil {
			text += "\nHạn trả lời: " + localExpiry(ch.ExpiresAt) + " (giờ Việt Nam)."
			if expiry, parseErr := time.Parse(time.RFC3339Nano, ch.ExpiresAt); parseErr == nil && !time.Now().Before(expiry) {
				text += " Mã đã hết hạn; đừng gửi mã cũ. Đang chờ kết quả dừng lần này."
			} else if ch.Status == "CONSUMING" {
				text += "\nĐã nhận câu trả lời, đang chờ ACB kiểm tra. Không gửi lại mã."
			} else if ch.PromptMessageID == 0 {
				text += "\nĐang gửi yêu cầu riêng; hãy chờ tin/ảnh đó trước khi trả lời."
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		} else if e.State == "WAITING_CAPTCHA" && e.AIUsed > e.CaptchaSubmissions {
			text = "2/6 · Đang đọc captcha tự động.\nBạn chờ; chưa cần nhập ký tự. Nếu cần bạn nhập tay, bot sẽ gửi ảnh yêu cầu riêng."
		} else {
			text += "\nĐang chờ ACB và chuẩn bị yêu cầu; chưa gửi mã vào chat."
		}
	case "VERIFYING":
		text = "5/6 · Đang xác minh phiên và đúng tài khoản ACB.\nChưa xác nhận thành công; bạn chờ, không cần gửi thêm mã."
	case "CATCHING_UP":
		text = "6/6 · Phiên đã xác minh, đang lấy giao dịch còn thiếu.\nChưa tiếp tục theo dõi cho đến khi lấy đủ giao dịch."
		if e.RecoveryRunID != "" {
			run, err := h.Store.GetRecoveryRun(ctx, e.RecoveryRunID)
			if err != nil {
				return "", err
			}
			text += "\nKhoảng " + run.RangeFrom + " đến " + run.RangeTo + "; ngày đang xử lý: " + run.NextDay + "."
		}
	case "COMPLETED":
		text = "Hoàn tất 6/6 · Đã xác minh ACB và lấy đủ giao dịch còn thiếu."
	case "CANCELLED", "CANCELED":
		text = "Đã hủy lần đăng nhập này. Bot không tự thử lại; bấm Đăng nhập nếu muốn bắt đầu lần mới."
	case "WAIT_OPERATOR", "MANUAL_REQUIRED", "FAILED", "RETRY_WAIT", "MAINTENANCE_WAIT":
		text = "Đã dừng lần đăng nhập này.\n" + reasonLabel(e.ReasonCode) + "\nBot không tự đăng nhập lại."
		if c.State == "MONITORING" {
			text = "Phiên đã xác minh nhưng chưa lấy đủ giao dịch.\n" + reasonLabel(e.ReasonCode) + "\nChưa tiếp tục theo dõi; không cần đăng nhập hoặc lấy OTP lại."
		}
	}
	if timed && progressKind(e.State) && e.AttemptID != "" && e.State != "CATCHING_UP" {
		attempt, err := h.Store.AuthAttemptStatusForOwner(ctx, e.AttemptID, storage.RecoveryOwner)
		if err == nil {
			if started, parseErr := time.Parse(time.RFC3339Nano, attempt.CreatedAt); parseErr == nil {
				seconds := int(time.Since(started).Seconds()) / 15 * 15
				if seconds < 0 {
					seconds = 0
				}
				text += fmt.Sprintf("\nĐã chờ khoảng %d giây · giới hạn lần này: %s (giờ Việt Nam).", seconds, localExpiry(attempt.ExpiresAt))
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	if timed && e.State == "CATCHING_UP" {
		if started, parseErr := time.Parse(time.RFC3339Nano, e.UpdatedAt); parseErr == nil {
			seconds := int(time.Since(started).Seconds()) / 15 * 15
			if seconds < 0 {
				seconds = 0
			}
			text += fmt.Sprintf("\nĐã chờ bước này khoảng %d giây. Tin này tự cập nhật; bạn không cần bấm làm mới.", seconds)
		}
	}
	settings, err := h.Store.GetMonitorSettings(ctx)
	if err != nil {
		return "", err
	}
	if !settings.Enabled || storage.ResolveSchedule(time.Now(), &settings).Mode == storage.ModePaused {
		text += "\nLịch theo dõi giao dịch đang tạm dừng."
	} else if e.State == "COMPLETED" {
		text += "\nĐã tiếp tục theo dõi giao dịch."
	}
	if h.AIDegraded != nil && h.AIDegraded() != "" {
		text += "\nĐọc captcha tự động đang không sẵn sàng. Khi được yêu cầu, trả lời trực tiếp ảnh captcha để tiếp tục."
	}
	return text, nil
}

func pendingConsent(e storage.AuthRecoveryEpisode) bool {
	expires, err := time.Parse(time.RFC3339Nano, e.ConsentExpiresAt)
	return e.ConsentActionID != "" && e.ConsentConsumedAt == "" && err == nil && time.Now().Before(expires)
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
func reasonLabel(reason string) string {
	switch reason {
	case "CREDENTIALS_REJECTED":
		return "ACB từ chối tên đăng nhập hoặc mật khẩu. Kiểm tra trong app ACB, rồi dùng Thông tin đăng nhập để lưu lại trước khi thử."
	case "CREDENTIALS_NOT_CONFIGURED", "CREDENTIALS_UNAVAILABLE":
		return "Chưa đọc được thông tin đăng nhập đã lưu. Dùng Thông tin đăng nhập để lưu lại hoặc nhờ người quản lý kiểm tra."
	case "CREDENTIALS_DECRYPT_FAILED":
		return "Không mở được thông tin đăng nhập đã mã hóa. Nhờ người quản lý kiểm tra khóa mã hóa, hoặc lưu lại qua Thông tin đăng nhập."
	case "CREDENTIALS_REVISION_CONFLICT":
		return "Thông tin đăng nhập vừa thay đổi. Kiểm tra thông tin mới rồi bấm Đăng nhập cho lần mới."
	case "ACCOUNT_LOCKED":
		return "ACB báo tài khoản bị khóa. Mở app ACB hoặc liên hệ ngân hàng để mở khóa trước khi thử lại."
	case "CATCHUP_FAILED", "HISTORY_RANGE_UNAVAILABLE":
		return "Lấy giao dịch còn thiếu thất bại. Bấm Thử lại lấy giao dịch thiếu nếu có; phiên hiện tại được giữ, không yêu cầu OTP mới."
	case "INVALID_CHECKPOINT", "CATCHUP_PROGRESS_MISSING":
		return "Mốc lấy giao dịch còn thiếu hoặc tiến độ đã lưu không hợp lệ. Nhờ người quản lý kiểm tra dữ liệu; không đăng nhập lại để bỏ qua lỗi."
	case "SESSION_DECRYPT_FAILED":
		return "Không mở được phiên ACB đã mã hóa. Nhờ người quản lý kiểm tra khóa mã hóa trước khi thử lại."
	case "ACCOUNT_SELECTION_REQUIRED", "ACCOUNT_SELECTION_PENDING":
		return "Chưa chọn và xác minh được đúng tài khoản ACB. Kiểm tra số tài khoản đã lưu; nhờ người quản lý kiểm tra trang chọn tài khoản nếu vẫn lỗi."
	case "UNSUPPORTED_CHALLENGE":
		return "ACB yêu cầu cách xác minh bot chưa hỗ trợ. Kiểm tra trong app ACB hoặc liên hệ ngân hàng; không gửi mã chuyển tiền vào chat."
	case "UNSAFE_CAPTCHA_CROP", "CAPTCHA_CAPTURE_UNAVAILABLE":
		return "Không lấy được ảnh captcha an toàn để gửi cho bạn. Nhờ người quản lý kiểm tra vùng captcha trên trang ACB; bot không gửi ảnh toàn trang chứa dữ liệu riêng."
	case "AMBIGUOUS_CONTROLS", "AMBIGUOUS_SUBMIT", "UNSAFE_CONTROLS":
		return "Không xác định chắc chắn ô nhập hoặc nút đăng nhập ACB. Bot dừng để tránh nhập sai; nhờ người quản lý kiểm tra giao diện ACB."
	case "FRAME_UNSUPPORTED":
		return "Trang đăng nhập ACB nằm trong khung bot chưa hỗ trợ. Nhờ người quản lý kiểm tra giao diện đăng nhập."
	case "WRONG_FORM_ORIGIN":
		return "Địa chỉ nhận thông tin đăng nhập không đúng trang ACB được cho phép. Bot đã dừng để bảo vệ tài khoản; nhờ người quản lý kiểm tra."
	case "UNKNOWN_PAGE", "UNRECOGNIZED_PAGE", "UNSUPPORTED_PAGE":
		return "Trang ACB không hiện bước đăng nhập mà bot nhận diện được trong thời gian chờ. Nhờ người quản lý kiểm tra trang ACB hoặc kết nối; chỉ thử lại sau khi đã kiểm tra."
	case "UNRECOGNIZED_REJECTION":
		return "ACB trả về thông báo từ chối bot chưa nhận diện được. Kiểm tra trong app ACB hoặc nhờ người quản lý kiểm tra; chưa xác nhận đăng nhập thành công."
	case "CAPTCHA_LOADING":
		return "Captcha ACB không tải xong trong thời gian chờ. Kiểm tra kết nối hoặc chờ ACB ổn định rồi bấm Đăng nhập lại."
	case "LOGIN_OUTCOME_UNKNOWN", "CHALLENGE_OUTCOME_UNKNOWN", "ACTION_OUTCOME_UNKNOWN":
		return "Chưa xác định được ACB có nhận thao tác vừa gửi hay không. Bot không gửi lại để tránh trùng; kiểm tra app ACB trước khi bấm Đăng nhập cho lần mới."
	case "OTP_REJECTED", "INVALID_OTP":
		return "ACB từ chối OTP đăng nhập. Khi thử lần mới, lấy mã mới từ app ACB và trả lời đúng tin yêu cầu còn hạn."
	case "CHALLENGE_EXPIRED", "OTP_EXPIRED", "ATTEMPT_EXPIRED", "ATTEMPT_TIMEOUT", "BROWSER_EXPIRED":
		return "Lần đăng nhập hoặc yêu cầu mã đã hết thời gian chờ. Không gửi mã cũ; bấm Đăng nhập để bắt đầu lần mới."
	case "CAPTCHA_BUDGET_EXHAUSTED", "CAPTCHA_REJECTED", "INVALID_CAPTCHA":
		return "Chưa vượt qua captcha ACB. Bấm Đăng nhập cho lần mới; nếu bot gửi ảnh, trả lời trực tiếp ảnh bằng ký tự hiện trên đó."
	case "ATTEMPT_BUDGET_EXHAUSTED":
		return "Các lần thử được cho phép đều chưa hoàn tất. Kiểm tra thông tin đăng nhập và app ACB trước khi tự chọn bắt đầu lần mới."
	case "BANK_MAINTENANCE", "MAINTENANCE":
		return "ACB đang bảo trì. Chờ ngân hàng hoạt động lại rồi bấm Đăng nhập; bot không tự thử."
	case "BROWSER_UNAVAILABLE":
		return "Không kết nối được trình duyệt đăng nhập ACB của hệ thống. Nhờ người quản lý kiểm tra dịch vụ và kết nối, rồi bấm Đăng nhập để thử lần mới."
	case "MANUAL_ACTIVE":
		return "Người quản lý đang đăng nhập ở nơi khác. Chờ lần đó kết thúc; bot không chiếm hoặc hủy lần đó."
	case "OPERATOR_CANCELLED":
		return "Bạn đã hủy lần này. Bấm Đăng nhập khi muốn bắt đầu lần mới."
	default:
		return "Chưa xác minh được phiên ACB. Nhờ người quản lý kiểm tra nguyên nhân trước khi chọn Đăng nhập cho lần mới; không gửi mật khẩu hay mã vào chat."
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
		if n.Kind != "CREDENTIALS_UPDATED" && (e.StatusMessageID > 0 || e.AttemptCount > 0) {
			err = h.deliverProgress(ctx)
			if err == nil {
				updated, readErr := h.Store.AuthRecoveryEpisode(ctx, e.ID)
				if readErr != nil {
					return readErr
				}
				beforeID = updated.StatusMessageID
			}
		} else {
			err = h.sendNoticePanel(ctx, c, e, text, &beforeID)
		}
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
	h.progressMu.Lock()
	defer h.progressMu.Unlock()
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	if e.ID == "" || !progressKind(e.State) && e.StatusMessageID == 0 && e.AttemptCount == 0 && e.ConsentActionID == "" {
		return nil
	}
	text, err := h.stateText(ctx, c, e, true)
	if err != nil {
		return err
	}
	p, err := h.planPanel(ctx, c, e, text)
	if err != nil {
		return err
	}
	// Only a control/state transition creates nonces. Elapsed-time edits reuse
	// the same message-bound controls; an expired click refreshes the panel.
	captchaRevision := ""
	if p.captcha != nil {
		captchaRevision = p.captcha.BrowserRevision
	}
	key := fmt.Sprintf("%s:%d:%d:%s:%s:%s:%v:%v", e.ID, c.Generation, e.ConfigRevision, e.AttemptID, e.State, captchaRevision, p.operations, p.keyboard.Rows)
	if h.progressEpisode == e.ID && h.progressText == p.text && h.progressKey == key {
		return nil
	}
	markup := h.progressMarkup
	var actions []storage.TelegramAuthAction
	if h.progressKey != key {
		markup, actions, err = h.renderPanel(ctx, c, e, p)
		if err != nil {
			return err
		}
	}
	id := e.StatusMessageID
	if id > 0 {
		err = h.Client.EditText(ctx, h.ChatID, id, p.text, markup)
		if err != nil {
			var transport *TransportError
			if !errors.As(err, &transport) || transport.Code != "TELEGRAM_MESSAGE_UNEDITABLE" {
				return err
			}
			id = 0
			// A replacement message needs fresh message-bound controls.
			if len(actions) == 0 {
				markup, actions, err = h.renderPanel(ctx, c, e, p)
				if err != nil {
					return err
				}
			}
		}
	}
	if id == 0 {
		id, err = h.Client.SendText(ctx, h.ChatID, p.text, markup)
		if err != nil {
			return err
		}
		if err := h.Store.SetAuthRecoveryStatusMessage(ctx, e.ID, c.Generation, id); err != nil {
			_ = h.Client.DeleteMessage(ctx, h.ChatID, id)
			return err
		}
	}
	if err := h.bindActions(ctx, actions, id); err != nil {
		return err
	}
	h.progressEpisode, h.progressText, h.progressKey, h.progressMarkup = e.ID, p.text, key, markup
	return nil
}
func (h *Handler) noticeText(ctx context.Context, n storage.AuthRecoveryNotice, e storage.AuthRecoveryEpisode) (string, error) {
	if n.Kind == "CREDENTIALS_UPDATED" {
		return "Đã lưu thông tin đăng nhập mới. Chưa đăng nhập ACB; bấm Đăng nhập khi sẵn sàng nhận OTP. Thao tác này không đổi mật khẩu tại ngân hàng.", nil
	}
	c, _, err := h.snapshot(ctx)
	if err != nil {
		return "", err
	}
	return h.stateText(ctx, c, e, false)
}
func (h *Handler) deliverLogoutNotices(ctx context.Context) error {
	jobs, err := h.Store.PendingACBLogoutNotices(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		local := "1/2 · Đang chờ xóa phiên khỏi hệ thống; chưa xác nhận hoàn tất."
		if job.LocalClearedAt != "" {
			local = "1/2 · Đã xóa phiên lưu và trạng thái bộ nhớ của hệ thống."
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
		text := local + "\n2/2 · " + bank + "\nHệ thống chưa đăng nhập lại. Cho phép đăng nhập không tạo lần mới."
		c, e, err := h.snapshot(ctx)
		if err != nil {
			return err
		}
		messageID := int64(0)
		if err := h.sendNoticePanel(ctx, c, e, text, &messageID); err != nil {
			return err
		}
		if err := h.Store.FinishACBLogoutNotice(ctx, job.ID, messageID, job.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}
