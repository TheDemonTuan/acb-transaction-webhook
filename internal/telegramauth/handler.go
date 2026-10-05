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
			return h.deliverProgress(ctx)
		}
		if errors.Is(err, challenge.ErrOutcomeUnknown) {
			h.wakeCoordinator()
			return h.replyExplanation(ctx, "⚠️ Chưa xác định được ACB có nhận mã hay không. Bot không gửi lại mã để tránh trùng.\n⏳ Vui lòng theo dõi kết quả ở tin tiến độ bên dưới.")
		}
		if rejectedDisposition(err) {
			h.wakeCoordinator()
			text := "⚠️ Yêu cầu nhập mã này không còn hiệu lực. Vui lòng làm theo thông báo mới nhất bên dưới."
			if errors.Is(err, storage.ErrChallengeExpired) {
				text = "⏱️ Mã đã hết thời gian chờ trả lời nên chưa được gửi tới ACB.\n💡 Vui lòng bấm 'Đăng nhập' để tạo lượt mới khi sẵn sàng."
			} else if errors.Is(err, storage.ErrChallengeConsumed) {
				text = "✅ Mã xác thực đã được tiếp nhận và đang xử lý.\n⏳ Vui lòng theo dõi kết quả ở tin tiến độ bên dưới."
			}
			return h.replyExplanation(ctx, text)
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

// Reply dispositions are finite explanations, not another mutable stage snapshot.
// The broker alone owns the immediate receipt; progress always uses its durable ID.
func (h *Handler) replyExplanation(ctx context.Context, text string) error {
	if err := h.say(ctx, text); err != nil {
		return err
	}
	return h.deliverProgress(ctx)
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
	active := progressKind(e.State) || pendingConsent(e)
	if active && !help {
		return h.updateProgress(ctx, true)
	}
	text, err := h.stateText(ctx, c, e, false)
	if err != nil {
		return err
	}
	if help {
		if active {
			text = ""
		}
		text += "\n\n📖 HƯỚNG DẪN SỬ DỤNG:\n" +
			"• Đăng nhập: Mỗi lần bấm chỉ thực hiện một lượt thử an toàn. Hệ thống không bao giờ tự ý đăng nhập ngầm.\n" +
			"• Nhập OTP & Captcha: Chỉ trả lời (Reply) trực tiếp vào tin nhắn yêu cầu riêng còn thời hạn. Tuyệt đối không gửi OTP chuyển tiền hay mật khẩu vào khung chat.\n" +
			"• Cập nhật thông tin: Dùng liên kết bảo mật để cập nhật tài khoản/mật khẩu lưu trên máy chủ, không làm thay đổi mật khẩu tại ngân hàng.\n" +
			"• An toàn: Dữ liệu được mã hóa đa tầng và xóa khỏi bộ nhớ ngay sau khi xác thực."
	}
	if active {
		// Help is static guidance, not another snapshot of the live attempt.
		return h.say(ctx, text)
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
		p.text += "\n\n⚠️ Hệ thống đang bảo trì; chỉ xem trạng thái và trợ giúp, chưa thể thực hiện thay đổi."
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
		p.text += "\n\n⚠️ Chưa lưu thông tin đăng nhập. Vui lòng bấm 'Thông tin đăng nhập' để cấu hình trước; không gửi mật khẩu vào chat."
	}
	if bot.Paused {
		p.text += "\n\n⏸️ Đăng nhập tự động đang tạm khóa. Bấm 'Cho phép đăng nhập' để mở lại."
	}
	if otherActive {
		p.text += "\n\n🔄 Đang có một lượt đăng nhập được thực hiện ở nơi khác. Bot đang chờ và không can thiệp vào phiên đó."
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
				p.text += "\n\n⏳ Có thể thử lại sau " + localExpiry(readyAt.Format(time.RFC3339Nano)) + " (giờ VN). Bấm 'Làm mới' khi đến giờ."
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
	var primaryRows [][]inlineButton
	var secondaryButtons []inlineButton
	for _, row := range keyboard.Rows {
		for _, btn := range row {
			if btn.Text == "Đăng nhập" || strings.Contains(btn.Text, "Thử lại") || strings.Contains(btn.Text, "Hủy") || strings.Contains(btn.Text, "Xem ảnh") {
				primaryRows = append(primaryRows, []inlineButton{btn})
			} else {
				secondaryButtons = append(secondaryButtons, btn)
			}
		}
	}
	for _, row := range p.keyboard.Rows {
		secondaryButtons = append(secondaryButtons, row...)
	}
	var finalRows [][]inlineButton
	finalRows = append(finalRows, primaryRows...)
	for i := 0; i < len(secondaryButtons); i += 2 {
		if i+1 < len(secondaryButtons) {
			finalRows = append(finalRows, []inlineButton{secondaryButtons[i], secondaryButtons[i+1]})
		} else {
			finalRows = append(finalRows, []inlineButton{secondaryButtons[i]})
		}
	}
	keyboard.Rows = finalRows
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
		return h.refreshPanel(ctx, "ℹ️ Phiên đã xác minh; hệ thống đang lấy các giao dịch còn thiếu.\nChỉ thử lại bước lấy giao dịch nếu đã thất bại; không cần đăng nhập lại.")
	}
	if (operation == "CANCEL") && e.RecoveryRunID != "" && c.State == "MONITORING" {
		return h.refreshPanel(ctx, "⚠️ Không thể hủy bước lấy giao dịch đã bắt đầu.\nPhiên đã xác minh được giữ an toàn.")
	}
	if operation == "LOGIN" || operation == "RETRY" {
		report, err := h.Store.ActiveAuthAttempts(ctx)
		if err != nil {
			return err
		}
		for _, a := range report.Attempts {
			if a.OwnerSubject != storage.RecoveryOwner {
				return h.refreshPanel(ctx, "🔄 Đang có phiên đăng nhập ở nơi khác. Bot đang chờ và không can thiệp vào phiên đó.")
			}
		}
		if e.State == "VERIFYING" || progressKind(e.State) && e.OTPSubmissions > 0 {
			return h.deliverProgress(ctx)
		}
		if e.AttemptID != "" {
			ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
			if err == nil {
				if ch.Status == "CONSUMING" || e.OTPSubmissions > 0 {
					return h.deliverProgress(ctx)
				}
				expiry, parseErr := time.Parse(time.RFC3339Nano, ch.ExpiresAt)
				if parseErr == nil && time.Now().Before(expiry) {
					return h.refreshPanel(ctx, "📲 Đang chờ mã xác thực còn thời hạn.\nVui lòng trả lời tin nhắn yêu cầu mã; không tạo thêm lần đăng nhập mới.")
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
	}
	var text string
	switch operation {
	case "LOGIN":
		text = "🔐 XÁC NHẬN BẮT ĐẦU ĐĂNG NHẬP ACB\n────────────────────────\nHệ thống sẽ mở trình duyệt để đăng nhập và xử lý captcha tự động.\n\n📲 Vui lòng chuẩn bị sẵn ứng dụng ACB ONE trên điện thoại để lấy mã OTP khi có yêu cầu.\n\n⏱️ Nút xác nhận có hiệu lực trong 60 giây:"
	case "LOGOUT":
		text = "🚪 XÁC NHẬN ĐĂNG XUẤT ACB\n────────────────────────\n⚠️ Thao tác này sẽ:\n• Xóa phiên đăng nhập đang lưu trên VPS.\n• Gửi yêu cầu thu hồi phiên an toàn tới máy chủ ACB.\n• Tạm dừng tự động đồng bộ giao dịch cho đến lần đăng nhập mới.\n\n⏱️ Nút xác nhận có hiệu lực trong 60 giây:"
	case "UPDATE_CREDENTIALS":
		text = "⚙️ CẬP NHẬT THÔNG TIN ĐĂNG NHẬP\n────────────────────────\nHệ thống sẽ tạo liên kết bảo mật (hạn 5 phút) để bạn cập nhật tài khoản và mật khẩu ngân hàng.\n• Yêu cầu đăng nhập tài khoản quản trị.\n• Chỉ lưu trữ trên VPS, không đổi mật khẩu tại ngân hàng.\n\n⏱️ Bấm xác nhận bên dưới để nhận liên kết:"
	case "CANCEL":
		text = "🛑 XÁC NHẬN HỦY LẦN ĐĂNG NHẬP\n────────────────────────\nBạn có chắc chắn muốn hủy lượt đăng nhập đang diễn ra không?\n\n⏱️ Nút xác nhận có hiệu lực trong 60 giây:"
	case "RETRY":
		text = "🔄 XÁC NHẬN THỬ LẠI LẤY GIAO DỊCH\n────────────────────────\nHệ thống sẽ kết nối lại phiên ACB hiện tại để tải các giao dịch còn thiếu.\n\n⏱️ Nút xác nhận có hiệu lực trong 60 giây:"
	default:
		text = fmt.Sprintf("⚠️ XÁC NHẬN THAO TÁC\n────────────────────────\nBạn có muốn thực hiện %s không?\n\n⏱️ Nút bấm có hiệu lực một lần trong 60 giây:", actionLabel(operation))
	}
	keyboard, actions, err := h.buttons(ctx, c, e, []string{operation})
	if err != nil {
		return err
	}
	keyboard.Rows = append(keyboard.Rows, []inlineButton{{Text: "Quay lại", Data: "nav:menu"}})
	id, err := h.Client.SendText(ctx, h.ChatID, text, keyboard)
	if err != nil {
		return err
	}
	for _, a := range actions {
		if err := h.Store.DeliverTelegramAuthAction(ctx, a.ID, id); err != nil {
			_ = h.Client.DeleteMessage(ctx, h.ChatID, id)
			return err
		}
	}
	return nil
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
	var a storage.TelegramAuthAction
	var disposition string
	var actionErr error
	callbackText := ""
	if strings.HasPrefix(q.Data, "ar:") && len(q.Data) == 25 {
		a, disposition, actionErr = h.Store.ConsumeTelegramAuthAction(ctx, strings.TrimPrefix(q.Data, "ar:"), h.Client.Readiness().BotID, h.ChatID, h.UserID, q.Message.ID, time.Now())
		if errors.Is(actionErr, storage.ErrChallengeExpired) {
			callbackText = "Nút đã hết hạn. Đang cập nhật nút mới trên tin tiến độ."
		} else if rejectedDisposition(actionErr) || errors.Is(actionErr, storage.ErrMutationGateLocked) {
			callbackText = operationFailure(actionErr)
		}
	}
	// Clear the spinner with finite feedback before any Telegram delivery work.
	ackCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_ = h.Client.AnswerCallbackQuery(ackCtx, q.ID, callbackText)
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
	err := actionErr
	if err != nil {
		if errors.Is(err, storage.ErrChallengeExpired) {
			return h.refreshPanel(ctx, "⏱️ Nút bấm đã hết hạn. Vui lòng dùng nút bấm mới bên dưới.")
		}
		if rejectedDisposition(err) || errors.Is(err, storage.ErrMutationGateLocked) {
			return h.refreshPanel(ctx, operationFailure(err))
		}
		return err
	}
	if disposition == "CREDENTIAL_GRANT" {
		msgText := "⚙️ LIÊN KẾT CẬP NHẬT THÔNG TIN\n────────────────────────\nLiên kết bảo mật đã sẵn sàng (có hạn trong 5 phút):\n👉 " + h.PublicOrigin + "/admin/acb-credentials#grant=" + a.CredentialGrantToken + "\n\n📝 Hướng dẫn thực hiện:\n1. Bấm mở liên kết trên trình duyệt web của bạn.\n2. Đăng nhập tài khoản quản trị để lưu tên đăng nhập & mật khẩu mới.\n3. Sau khi lưu xong, quay lại đây bấm 'Đăng nhập' để kiểm tra kết nối.\n\n🛡️ Thao tác chỉ lưu thông tin bảo mật trên VPS, không đổi mật khẩu tại ngân hàng."
		_, err := h.Client.SendText(ctx, h.ChatID, msgText, inlineKeyboard{Rows: [][]inlineButton{{{Text: "Quay lại ACB", Data: "nav:menu"}}}})
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
		return "⚠️ Hệ thống đang bảo trì. Bạn vẫn có thể xem Trạng thái và Hướng dẫn."
	case errors.Is(err, storage.ErrRecoveryCommitted):
		return "⚠️ Không thể hủy khi đang trong tiến trình đồng bộ dữ liệu. Phiên đăng nhập hiện tại vẫn được bảo vệ an toàn."
	case errors.Is(err, storage.ErrRecoveryCooldown):
		return "⏳ Chưa đủ 60 giây từ lần gửi đăng nhập trước. Vui lòng chờ ít giây rồi xác nhận lại."
	case errors.Is(err, storage.ErrAuthAttemptActive):
		return "🔄 Đang có một lượt đăng nhập đang xử lý. Vui lòng theo dõi tiến độ hoặc trả lời tin nhắn yêu cầu mã OTP/Captcha."
	default:
		return "ℹ️ Nút bấm đã được sử dụng hoặc trạng thái vừa thay đổi. Vui lòng dùng nút bấm mới bên dưới."
	}
}
func (h *Handler) refreshPanel(ctx context.Context, result string) error {
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return err
	}
	if progressKind(e.State) || pendingConsent(e) {
		// Stale controls must not leave a frozen copy of an earlier stage in chat.
		return h.updateProgress(ctx, true)
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
		return h.refreshPanel(ctx, "🚪 ĐÃ NHẬN YÊU CẦU ĐĂNG XUẤT\n────────────────────────\n• Bước 1: Đang xóa phiên lưu trữ trên hệ thống.\n• Bước 2: Đang gửi lệnh thu hồi phiên an toàn tới ACB.\n\n💡 Kết quả thu hồi sẽ được thông báo ngay khi hoàn tất. Hệ thống không tự động đăng nhập lại.")
	case "STATUS", "ACTIVE":
		return h.status(ctx)
	case "CATCHUP_RETRY", "REARMED":
		return h.deliverProgress(ctx)
	case "PAUSE":
		return h.refreshPanel(ctx, "⏸️ ĐÃ TẠM KHÓA ĐĂNG NHẬP\n────────────────────────\nĐã hủy các phiên đăng nhập đang chờ và tạm dừng đăng nhập tự động.\n💡 Nếu phiên ngân hàng đang hoạt động, hệ thống vẫn tiếp tục theo dõi giao dịch theo lịch.")
	case "RESUMED":
		return h.refreshPanel(ctx, "▶️ ĐÃ MỞ KHÓA ĐĂNG NHẬP\n────────────────────────\nHệ thống đã sẵn sàng cho phép đăng nhập mới.\n💡 Thao tác này không tự động đăng nhập. Bấm 'Đăng nhập' khi bạn muốn bắt đầu.")
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
		return h.sendPanel(ctx, c, e, "🛑 ĐÃ HỦY LẦN ĐĂNG NHẬP\n────────────────────────\nĐã hủy phiên làm việc hiện tại an toàn.\n💡 Hệ thống không tự động thử lại. Bấm 'Đăng nhập' khi bạn muốn bắt đầu lượt mới.")
	default:
		return errors.New("TELEGRAM_OPERATION_INVALID")
	}
}
func (h *Handler) status(ctx context.Context) error {
	return h.menu(ctx, false)
}

func (h *Handler) stateText(ctx context.Context, c storage.Connection, e storage.AuthRecoveryEpisode, timed bool) (string, error) {
	accountDisplay := c.AccountMasked
	if accountDisplay == "" {
		accountDisplay = "Chưa thiết lập"
	}
	monitorStatus := "🟢 Tự động đồng bộ"
	settings, err := h.Store.GetMonitorSettings(ctx)
	if err != nil {
		return "", err
	}
	if !settings.Enabled || storage.ResolveSchedule(time.Now(), &settings).Mode == storage.ModePaused {
		monitorStatus = "⏸️ Tạm dừng theo lịch"
	}

	var text string
	switch e.State {
	case "DETECTED":
		if pendingConsent(e) {
			text = "🔄 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đã tiếp nhận yêu cầu đăng nhập\n💡 Đang chờ khởi động phiên ACB. Bạn chưa cần gửi mã, vui lòng chờ trong giây lát."
		} else if e.ConsentActionID != "" && e.ConsentConsumedAt == "" {
			text = fmt.Sprintf("🏦 QUẢN LÝ PHIÊN ACB\n────────────────────────\n📌 Trạng thái: %s\n👤 Số tài khoản: %s\n📡 Giám sát: %s\n\n⚠️ Yêu cầu đăng nhập trước đã hết hạn.\n👉 Bấm 'Đăng nhập' bên dưới để tạo lượt đăng nhập mới.", connectionLabel(c.State), accountDisplay, monitorStatus)
		} else {
			guidance := "💡 Phiên ngân hàng chưa kết nối. Bấm 'Đăng nhập' bên dưới khi bạn sẵn sàng mở app ACB lấy mã OTP."
			if c.State == "MONITORING" {
				guidance = "✨ Hệ thống đang kết nối ngân hàng ổn định và theo dõi biến động số dư theo thời gian thực."
			}
			text = fmt.Sprintf("🏦 QUẢN LÝ PHIÊN ACB\n────────────────────────\n📌 Trạng thái: %s\n👤 Số tài khoản: %s\n📡 Giám sát: %s\n\n%s", connectionLabel(c.State), accountDisplay, monitorStatus, guidance)
		}
	case "STARTING":
		text = "🔄 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đang khởi động trình duyệt bảo mật...\n\n  🔄 [1/5] Khởi động phiên bảo mật ACB\n  ⬜ [2/5] Xử lý mã Captcha\n  ⬜ [3/5] Gửi thông tin đăng nhập\n  ⬜ [4/5] Xác thực mã OTP\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n💡 Hệ thống đang mở trang ACB an toàn; tiến độ sẽ tự cập nhật trong tin nhắn này."
	case "LOGIN":
		loginAt, loginErr := time.Parse(time.RFC3339Nano, e.LastLoginAt)
		consentAt, consentErr := time.Parse(time.RFC3339Nano, e.ConsentConsumedAt)
		switch {
		case e.ReasonCode == "OTP_REQUEST_SENT":
			text = "📲 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đang xác nhận phương thức OTP...\n\n  ✅ [1/5] Khởi động phiên bảo mật\n  ✅ [2/5] Xử lý Captcha thành công\n  ✅ [3/5] Đã gửi thông tin đăng nhập\n  🔄 [4/5] Đang yêu cầu ACB gửi mã OTP...\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n💡 Ngân hàng đang tạo mã OTP đăng nhập. Bạn chưa cần gửi mã; hãy chờ tin nhắn yêu cầu riêng từ bot."
		case loginErr == nil && consentErr == nil && !loginAt.Before(consentAt):
			text = "🔐 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đang xác thực thông tin tài khoản...\n\n  ✅ [1/5] Khởi động phiên bảo mật\n  ✅ [2/5] Xử lý Captcha thành công\n  🔄 [3/5] Đang gửi thông tin đăng nhập và chờ ACB phản hồi...\n  ⬜ [4/5] Xác thực mã OTP\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n💡 Đang xác thực thông tin an toàn với ACB; vui lòng không gửi lại mật khẩu."
		case e.AIUsed > 0:
			text = "🧩 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đang nhận diện mã Captcha...\n\n  ✅ [1/5] Khởi động phiên bảo mật\n  🔄 [2/5] Đang nhận diện Captcha tự động...\n  ⬜ [3/5] Gửi thông tin đăng nhập\n  ⬜ [4/5] Xác thực mã OTP\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n💡 Hệ thống đang tự động đọc captcha. Nếu cần nhập tay, bot sẽ gửi ảnh riêng để bạn trả lời."
		default:
			text = "🌐 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đang kết nối tới cổng đăng nhập ACB...\n\n  🔄 [1/5] Đang chờ trang đăng nhập ACB sẵn sàng...\n  ⬜ [2/5] Xử lý mã Captcha\n  ⬜ [3/5] Gửi thông tin đăng nhập\n  ⬜ [4/5] Xác thực mã OTP\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n💡 Đang tải giao diện đăng nhập ngân hàng; bạn chưa cần gửi mã."
		}
	case "WAITING_CAPTCHA", "WAITING_OTP":
		ch, err := h.Store.ActiveAuthChallenge(ctx, e.AttemptID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if e.State == "WAITING_OTP" && e.OTPSubmissions > 0 || err == nil && ch.Status == "CONSUMING" {
			text = "⏳ TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đang kiểm tra mã xác thực với ngân hàng...\n\n  ✅ [1/5] Khởi động phiên bảo mật\n  ✅ [2/5] Xử lý Captcha thành công\n  ✅ [3/5] Thông tin đăng nhập hợp lệ\n  🔄 [4/5] Đã nhận mã, ACB đang xác thực...\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n💡 Đang chờ ACB kiểm tra mã xác thực; vui lòng không gửi lại mã."
		} else if err == nil {
			expiry, parseErr := time.Parse(time.RFC3339Nano, ch.ExpiresAt)
			if parseErr != nil || !time.Now().Before(expiry) {
				text = "⏱️ Yêu cầu mã đã hết thời gian chờ.\nKhông gửi mã cũ; hệ thống đang dừng lượt này để bảo vệ tài khoản."
			} else if ch.PromptMessageID == 0 {
				text = "📲 Đang chuẩn bị yêu cầu mã xác thực riêng; vui lòng chờ trong giây lát..."
			} else if e.State == "WAITING_OTP" {
				text = "📲 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: CHỜ NHẬP MÃ OTP ĐĂNG NHẬP\n\n  ✅ [1/5] Khởi động phiên bảo mật\n  ✅ [2/5] Xử lý Captcha thành công\n  ✅ [3/5] Thông tin đăng nhập hợp lệ\n  🔔 [4/5] Đang chờ mã OTP từ bạn...\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n👉 Hướng dẫn nhập OTP:\n1. Mở app ACB ONE trên điện thoại lấy mã OTP đăng nhập.\n2. Bấm 'Trả lời' (Reply) vào tin nhắn yêu cầu OTP riêng bên dưới.\n⚠️ Giữ nguyên chữ số 0 ở đầu (nếu có). Không dùng OTP chuyển tiền!\n⏱️ Hạn trả lời: " + localExpiry(ch.ExpiresAt) + " (giờ VN)"
			} else {
				text = "🧩 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: CHỜ BẠN NHẬP MÃ CAPTCHA\n\n  ✅ [1/5] Khởi động phiên bảo mật\n  🔔 [2/5] Đang chờ bạn nhập mã Captcha...\n  ⬜ [3/5] Gửi thông tin đăng nhập\n  ⬜ [4/5] Xác thực mã OTP\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n👉 Hướng dẫn nhập:\nXem ảnh captcha gửi riêng bên dưới, bấm 'Trả lời' (Reply) ảnh đó bằng các ký tự nhìn thấy.\n⏱️ Hạn trả lời: " + localExpiry(ch.ExpiresAt) + " (giờ VN)"
			}
		} else if e.State == "WAITING_CAPTCHA" && e.AIUsed > e.CaptchaSubmissions {
			text = "🧩 TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đang nhận diện mã Captcha...\n\n  ✅ [1/5] Khởi động phiên bảo mật\n  🔄 [2/5] Đang nhận diện Captcha tự động...\n  ⬜ [3/5] Gửi thông tin đăng nhập\n  ⬜ [4/5] Xác thực mã OTP\n  ⬜ [5/5] Hoàn tất & Kiểm tra phiên\n\n💡 Hệ thống đang tự động đọc captcha. Nếu cần bạn nhập tay, bot sẽ gửi ảnh yêu cầu riêng."
		} else {
			text = "⏳ Đang chuẩn bị yêu cầu xác thực từ ACB; bạn chưa cần gửi mã vào chat."
		}
	case "VERIFYING":
		verifyDeadlineText := ""
		started, err := h.Store.RecoveryVerificationStartedAt(ctx, e.ID, e.Generation)
		if errors.Is(err, storage.ErrRecoverySuperseded) {
			return "", err
		}
		if err != nil {
			verifyDeadlineText = "\n⚠️ " + reasonLabel("VERIFICATION_STATE_INVALID")
		} else {
			deadline := started.Add(60 * time.Second)
			attempt, err := h.Store.AuthAttemptStatusForOwner(ctx, e.AttemptID, storage.RecoveryOwner)
			if err != nil {
				return "", err
			}
			expiry, err := time.Parse(time.RFC3339Nano, attempt.ExpiresAt)
			if err != nil {
				verifyDeadlineText = "\n⚠️ " + reasonLabel("VERIFICATION_STATE_INVALID")
			} else {
				if expiry.Before(deadline) {
					deadline = expiry
				}
				verifyDeadlineText = "\n⏱️ Hạn xác minh: " + localExpiry(deadline.UTC().Format(time.RFC3339Nano)) + " (giờ VN)"
			}
		}
		text = "🛡️ TIẾN TRÌNH ĐĂNG NHẬP ACB\n────────────────────────\n⏳ Trạng thái: Đang xác minh phiên và kiểm tra tài khoản..." + verifyDeadlineText + "\n\n  ✅ [1/5] Khởi động phiên bảo mật\n  ✅ [2/5] Xử lý Captcha thành công\n  ✅ [3/5] Thông tin đăng nhập hợp lệ\n  ✅ [4/5] Đã nhận mã OTP thành công\n  🔄 [5/5] Đối soát tài khoản & Thiết lập phiên kết nối...\n\n💡 Đang kiểm tra đúng tài khoản ngân hàng và khởi tạo phiên an toàn; không gửi thêm mã."
	case "CATCHING_UP":
		if e.RecoveryRunID != "" {
			run, err := h.Store.GetRecoveryRun(ctx, e.RecoveryRunID)
			if err != nil {
				return "", err
			}
			if run.Status == storage.RecoveryRunStatusFailed || run.Status == storage.RecoveryRunStatusCanceled {
				reason := "CATCHUP_FAILED"
				if run.ErrorCode == "INVALID_CHECKPOINT" {
					reason = "INVALID_CHECKPOINT"
				}
				text = "⚠️ ĐỒNG BỘ GIAO DỊCH GẶP GIÁN ĐOẠN\n────────────────────────\nPhiên đã xác minh nhưng bước lấy giao dịch chưa hoàn tất.\n\n📌 Chi tiết:\n" + reasonLabel(reason) + "\n\n🛠️ Hướng xử lý:\n• " + reasonAction(reason) + "\n\n💡 Phiên đăng nhập vẫn an toàn; không cần đăng nhập hoặc lấy OTP lại."
			} else {
				text = fmt.Sprintf("📊 ĐỒNG BỘ DỮ LIỆU GIAO DỊCH\n────────────────────────\n🎉 Phiên ACB đã xác thực thành công!\n🔄 Đang tải các giao dịch còn thiếu:\n  • Khoảng thời gian: %s ➔ %s\n  • Ngày đang xử lý: %s\n\n⏳ Tiến trình đang chạy tự động trong nền; tin nhắn sẽ tự cập nhật khi hoàn tất.", run.RangeFrom, run.RangeTo, run.NextDay)
			}
		} else {
			text = "📊 ĐỒNG BỘ DỮ LIỆU GIAO DỊCH\n────────────────────────\n🎉 Phiên ACB đã xác thực thành công!\n🔄 Đang tải các giao dịch còn thiếu trong thời gian gián đoạn...\n⏳ Quá trình diễn ra tự động trong nền."
		}
	case "COMPLETED":
		text = fmt.Sprintf("🎉 ĐĂNG NHẬP & KẾT NỐI ACB THÀNH CÔNG!\n────────────────────────\n✅ Phiên ngân hàng đã được xác thực an toàn.\n✅ Đã kiểm tra đúng tài khoản: %s.\n✅ Dữ liệu giao dịch đã được đồng bộ đầy đủ.\n📡 Hệ thống đang tự động theo dõi biến động số dư theo thời gian thực.", accountDisplay)
	case "CANCELLED", "CANCELED":
		text = "🛑 ĐÃ HỦY LẦN ĐĂNG NHẬP\n────────────────────────\nPhiên đăng nhập này đã được hủy an toàn.\n💡 Hệ thống không tự động thử lại. Bấm 'Đăng nhập' bên dưới khi bạn muốn bắt đầu lượt mới."
	case "WAIT_OPERATOR", "MANUAL_REQUIRED", "FAILED", "RETRY_WAIT", "MAINTENANCE_WAIT":
		reason := e.ReasonCode
		if reason == "" {
			reason = "UNKNOWN"
		}
		if c.State == "MONITORING" {
			text = "⚠️ ĐỒNG BỘ GIAO DỊCH GẶP GIÁN ĐOẠN\n────────────────────────\nPhiên đã xác minh nhưng bước lấy giao dịch chưa hoàn tất.\n\n📌 Chi tiết:\n" + reasonLabel(reason) + "\n\n🛠️ Hướng xử lý:\n• " + reasonAction(reason) + "\n\n💡 Phiên đăng nhập vẫn an toàn; không cần đăng nhập hoặc lấy OTP lại."
		} else {
			text = "❌ ĐĂNG NHẬP CHƯA THÀNH CÔNG\n────────────────────────\n📌 Nguyên nhân:\n" + reasonLabel(reason) + "\n\n🛠️ Hướng xử lý:\n• " + reasonAction(reason) + "\n\n💡 Hệ thống đã dừng phiên an toàn để bảo vệ tài khoản của bạn. Bấm 'Đăng nhập' bên dưới để thử lại."
		}
	}

	if timed && progressKind(e.State) && e.AttemptID != "" && e.State != "CATCHING_UP" && e.State != "VERIFYING" {
		attempt, err := h.Store.AuthAttemptStatusForOwner(ctx, e.AttemptID, storage.RecoveryOwner)
		if err == nil {
			if started, parseErr := time.Parse(time.RFC3339Nano, attempt.CreatedAt); parseErr == nil {
				seconds := int(time.Since(started).Seconds()) / 15 * 15
				if seconds < 0 {
					seconds = 0
				}
				text += fmt.Sprintf("\n\n⏱️ Đã xử lý khoảng %d giây · Hạn chót: %s (giờ VN).", seconds, localExpiry(attempt.ExpiresAt))
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
			text += fmt.Sprintf("\n\n⏱️ Đang đồng bộ khoảng %d giây. Tin nhắn này tự động cập nhật.", seconds)
		}
	}
	if h.AIDegraded != nil && h.AIDegraded() != "" {
		text += "\n\n⚠️ Đọc captcha tự động hiện không sẵn sàng. Nếu cần thiết, bot sẽ gửi ảnh captcha để bạn nhập tay."
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
		return "🟢 Đang hoạt động (Đã xác minh)"
	case "AUTH_REQUIRED":
		return "🟡 Cần đăng nhập lại"
	case "AUTH_STARTING":
		return "🔵 Đang xử lý đăng nhập"
	case "UNCONFIGURED":
		return "⚪ Chưa cấu hình tài khoản"
	default:
		return "⚪ Chưa sẵn sàng"
	}
}

func reasonAction(reason string) string {
	switch reason {
	case "VERIFICATION_TIMEOUT":
		return "Mở app ACB ONE kiểm tra thông báo phiên, sau đó bấm Đăng nhập để lấy mã OTP mới."
	case "VERIFICATION_ACCOUNT_MISMATCH":
		return "Kiểm tra lại số tài khoản ngân hàng trong mục Thông tin đăng nhập."
	case "VERIFICATION_ACCOUNT_MISSING", "VERIFICATION_PAGE_UNSUPPORTED", "FRAME_UNSUPPORTED", "AMBIGUOUS_CONTROLS", "AMBIGUOUS_SUBMIT", "UNKNOWN_PAGE", "UNRECOGNIZED_PAGE", "UNSUPPORTED_PAGE":
		return "Bấm Đăng nhập để thử lại. Nếu vẫn gặp lỗi này, nhờ quản trị viên kiểm tra lại kết nối giao diện ACB."
	case "VERIFICATION_AUTH_REQUIRED", "OTP_REJECTED", "INVALID_OTP":
		return "Bấm Đăng nhập, mở app ACB ONE lấy mã OTP mới và trả lời ngay khi bot gửi yêu cầu."
	case "CREDENTIALS_REJECTED":
		return "Mở mục Thông tin đăng nhập để cập nhật lại tên đăng nhập hoặc mật khẩu ACB ONE."
	case "ACCOUNT_LOCKED":
		return "Mở app ACB ONE hoặc liên hệ tổng đài ACB (1900 54 54 86) để mở khóa tài khoản trước."
	case "BANK_MAINTENANCE", "MAINTENANCE", "VERIFICATION_MAINTENANCE":
		return "Vui lòng chờ ngân hàng hoàn tất bảo trì rồi bấm Đăng nhập lại."
	case "CHALLENGE_EXPIRED", "OTP_EXPIRED", "ATTEMPT_EXPIRED", "ATTEMPT_TIMEOUT", "BROWSER_EXPIRED":
		return "Bấm Đăng nhập khi bạn đã sẵn sàng cầm điện thoại để nhận mã."
	case "CAPTCHA_BUDGET_EXHAUSTED", "CAPTCHA_REJECTED", "INVALID_CAPTCHA":
		return "Bấm Đăng nhập lại. Nếu bot gửi ảnh captcha, hãy nhập đúng các ký tự trong ảnh."
	case "CATCHUP_FAILED", "HISTORY_RANGE_UNAVAILABLE":
		return "Phiên làm việc vẫn an toàn. Bấm 'Thử lại lấy giao dịch thiếu' để hệ thống đồng bộ lại."
	case "OPERATOR_CANCELLED":
		return "Bấm Đăng nhập khi bạn muốn bắt đầu lượt mới."
	default:
		return "Bấm Đăng nhập để thử lại lượt mới."
	}
}

func reasonLabel(reason string) string {
	switch reason {
	case "VERIFICATION_ACCOUNT_MISMATCH":
		return "Số tài khoản từ ACB không khớp với số tài khoản đã lưu trên hệ thống."
	case "VERIFICATION_ACCOUNT_MISSING":
		return "Trang lịch sử ACB không phản hồi số tài khoản để đối soát."
	case "VERIFICATION_FORM_INVALID":
		return "Không đọc được cấu trúc biểu mẫu ACB để xác minh phiên."
	case "VERIFICATION_AUTH_REQUIRED":
		return "ACB yêu cầu xác thực lại hoặc chưa lưu phiên đăng nhập."
	case "VERIFICATION_PAGE_UNSUPPORTED":
		return "ACB phản hồi trang chưa được nhận diện để xác minh phiên."
	case "VERIFICATION_MAINTENANCE":
		return "Ngân hàng ACB đang trong thời gian bảo trì hệ thống."
	case "VERIFICATION_UNAVAILABLE":
		return "Dịch vụ chưa thể kết nối xác minh phiên với máy chủ ACB."
	case "VERIFICATION_SUPERSEDED":
		return "Phiên làm việc này đã được thay thế bởi thao tác mới hơn."
	case "VERIFICATION_TIMEOUT":
		return "Quá thời gian chờ phản hồi xác minh từ ACB (60 giây). Hệ thống không kết luận mã OTP sai."
	case "VERIFICATION_STATE_INVALID":
		return "Mốc thời gian xác minh lưu trữ không hợp lệ hoặc bị thiếu."
	case "CREDENTIALS_REJECTED":
		return "ACB từ chối tên đăng nhập hoặc mật khẩu đã cung cấp."
	case "CREDENTIALS_NOT_CONFIGURED", "CREDENTIALS_UNAVAILABLE":
		return "Chưa cấu hình thông tin đăng nhập tài khoản ACB trên hệ thống."
	case "CREDENTIALS_DECRYPT_FAILED":
		return "Không thể giải mã thông tin đăng nhập tài khoản ACB."
	case "CREDENTIALS_REVISION_CONFLICT":
		return "Thông tin đăng nhập ACB vừa được cập nhật phiên bản mới."
	case "ACCOUNT_LOCKED":
		return "ACB thông báo tài khoản đang bị tạm khóa bảo vệ."
	case "CATCHUP_FAILED", "HISTORY_RANGE_UNAVAILABLE":
		return "Quá trình tải dữ liệu giao dịch còn thiếu gặp gián đoạn."
	case "INVALID_CHECKPOINT", "CATCHUP_PROGRESS_MISSING":
		return "Mốc đồng bộ dữ liệu giao dịch đã lưu không hợp lệ."
	case "SESSION_DECRYPT_FAILED":
		return "Không thể mở khóa phiên đăng nhập ACB đã lưu trữ."
	case "ACCOUNT_SELECTION_REQUIRED", "ACCOUNT_SELECTION_PENDING":
		return "Chưa chọn được đúng tài khoản ACB mục tiêu."
	case "UNSUPPORTED_CHALLENGE":
		return "ACB yêu cầu phương thức xác thực chưa được hỗ trợ."
	case "UNSAFE_CAPTCHA_CROP", "CAPTCHA_CAPTURE_UNAVAILABLE":
		return "Không thể trích xuất ảnh Captcha bảo mật từ trang ACB."
	case "AMBIGUOUS_CONTROLS", "AMBIGUOUS_SUBMIT", "UNSAFE_CONTROLS":
		return "Không thể xác định nút bấm đăng nhập an toàn trên giao diện ACB."
	case "FRAME_UNSUPPORTED":
		return "Khung giao diện đăng nhập ACB chưa được hỗ trợ."
	case "WRONG_FORM_ORIGIN":
		return "Địa chỉ gửi thông tin đăng nhập không khớp với tên miền ACB."
	case "UNKNOWN_PAGE", "UNRECOGNIZED_PAGE", "UNSUPPORTED_PAGE":
		return "Giao diện ACB không hiển thị bước đăng nhập hợp lệ trong thời gian chờ."
	case "UNRECOGNIZED_REJECTION":
		return "ACB từ chối đăng nhập với thông báo chưa được hỗ trợ."
	case "CAPTCHA_LOADING":
		return "Mã Captcha ACB không tải xong trong thời gian cho phép."
	case "LOGIN_OUTCOME_UNKNOWN", "CHALLENGE_OUTCOME_UNKNOWN", "ACTION_OUTCOME_UNKNOWN":
		return "Chưa xác định được kết quả gửi thao tác tới ACB."
	case "OTP_REQUEST_OUTCOME_UNKNOWN":
		return "Chưa xác định được ACB đã tiếp nhận yêu cầu gửi OTP hay chưa."
	case "OTP_REJECTED", "INVALID_OTP":
		return "Mã OTP không chính xác hoặc đã bị ngân hàng từ chối."
	case "CHALLENGE_EXPIRED", "OTP_EXPIRED", "ATTEMPT_EXPIRED", "ATTEMPT_TIMEOUT", "BROWSER_EXPIRED":
		return "Quá thời hạn chờ thao tác hoặc phiên đăng nhập đã hết hạn."
	case "CAPTCHA_BUDGET_EXHAUSTED", "CAPTCHA_REJECTED", "INVALID_CAPTCHA":
		return "Chưa vượt qua được bước xác thực mã Captcha của ACB."
	case "ATTEMPT_BUDGET_EXHAUSTED":
		return "Đã hết số lượt thử đăng nhập được phép trong phiên này."
	case "BANK_MAINTENANCE", "MAINTENANCE":
		return "Ngân hàng ACB đang trong thời gian bảo trì định kỳ."
	case "BROWSER_UNAVAILABLE":
		return "Không thể khởi động trình duyệt bảo mật trên máy chủ."
	case "MANUAL_ACTIVE":
		return "Đang có phiên đăng nhập thủ công khác đang diễn ra."
	case "OPERATOR_CANCELLED":
		return "Bạn đã chủ động hủy lượt đăng nhập này."
	default:
		return "Chưa thể thiết lập phiên kết nối ACB an toàn."
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
		useProgress := e.StatusMessageID > 0 || e.AttemptCount > 0
		switch n.Kind {
		case "COMPLETED", "WAIT_OPERATOR", "MANUAL_REQUIRED", "FAILED", "CANCELLED", "CANCELED", "RETRY_WAIT", "MAINTENANCE_WAIT":
			useProgress = true
		}
		messageID := int64(0)
		if n.Kind == "CREDENTIALS_UPDATED" {
			text := "⚙️ THÔNG TIN ĐĂNG NHẬP ĐÃ LƯU\n────────────────────────\n✅ Đã lưu thông tin tài khoản ACB mới vào hệ thống.\n💡 Chưa đăng nhập ACB; bấm 'Đăng nhập' khi bạn sẵn sàng nhận OTP.\n🛡️ Thao tác này không làm thay đổi mật khẩu tại ngân hàng."
			err = h.sendNoticePanel(ctx, c, e, text, &messageID)
		} else if useProgress {
			// Terminal results advance the existing panel, including when it must
			// be created or replaced. The notice acknowledges its canonical ID.
			messageID, err = h.updateProgressPanel(ctx, false, &e)
			if err == nil {
				latestConnection, latestEpisode, readErr := h.snapshot(ctx)
				if readErr != nil {
					err = readErr
				} else if !progressFenceMatches(latestConnection, latestEpisode, e) || latestEpisode.StatusMessageID != messageID {
					err = storage.ErrRecoverySuperseded
				}
			}
		} else {
			text, textErr := h.stateText(ctx, c, e, false)
			if textErr != nil {
				return textErr
			}
			err = h.sendNoticePanel(ctx, c, e, text, &messageID)
		}
		if err != nil {
			if persistErr := h.Store.FinishAuthRecoveryNotice(ctx, n.ID, 0, time.Now().Add(deliveryDelay(err))); persistErr != nil {
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

func (h *Handler) deliverProgress(ctx context.Context) error {
	return h.updateProgress(ctx, false)
}

func (h *Handler) updateProgress(ctx context.Context, refreshControls bool) error {
	_, err := h.updateProgressPanel(ctx, refreshControls, nil)
	return err
}

func progressFenceMatches(c storage.Connection, e, expected storage.AuthRecoveryEpisode) bool {
	return c.ID == expected.ConnectionID && c.Generation == expected.Generation && e.ID == expected.ID && e.Generation == expected.Generation && e.ConfigRevision == expected.ConfigRevision && e.AttemptID == expected.AttemptID && e.AttemptCount == expected.AttemptCount && e.State == expected.State
}

func (h *Handler) updateProgressPanel(ctx context.Context, refreshControls bool, expected *storage.AuthRecoveryEpisode) (int64, error) {
	h.progressMu.Lock()
	defer h.progressMu.Unlock()
	c, e, err := h.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	if expected != nil && !progressFenceMatches(c, e, *expected) {
		return 0, storage.ErrRecoverySuperseded
	}
	if e.ID == "" || expected == nil && !progressKind(e.State) && e.StatusMessageID == 0 && e.AttemptCount == 0 && e.ConsentActionID == "" {
		return 0, nil
	}
	text, err := h.stateText(ctx, c, e, true)
	if err != nil {
		return 0, err
	}
	p, err := h.planPanel(ctx, c, e, text)
	if err != nil {
		return 0, err
	}
	// Only a control/state transition creates nonces. Elapsed-time edits reuse
	// the same message-bound controls; an expired click refreshes the panel.
	captchaRevision := ""
	if p.captcha != nil {
		captchaRevision = p.captcha.BrowserRevision
	}
	key := fmt.Sprintf("%s:%d:%d:%s:%s:%s:%v:%v", e.ID, c.Generation, e.ConfigRevision, e.AttemptID, e.State, captchaRevision, p.operations, p.keyboard.Rows)
	if !refreshControls && h.progressEpisode == e.ID && h.progressText == p.text && h.progressKey == key {
		return e.StatusMessageID, nil
	}
	if expected == nil && !refreshControls && !progressKind(e.State) {
		// Background progress must not bypass a terminal notice's persisted
		// transport deadline, including after a process restart.
		notices, err := h.Store.PendingAuthRecoveryNotices(ctx)
		if err != nil {
			return 0, err
		}
		for _, n := range notices {
			if n.EpisodeID != e.ID || n.Kind != e.State || n.EventKey != fmt.Sprintf("%s:%s:%d", e.ID, e.State, e.AttemptCount) || n.NextAttemptAt == "" {
				continue
			}
			when, err := time.Parse(time.RFC3339Nano, n.NextAttemptAt)
			if err != nil {
				return 0, errors.New("TELEGRAM_NOTICE_RETRY_INVALID")
			}
			if time.Now().Before(when) {
				return e.StatusMessageID, nil
			}
		}
	}
	if refreshControls {
		h.progressKey = ""
	}
	markup := h.progressMarkup
	var actions []storage.TelegramAuthAction
	if h.progressKey != key {
		markup, actions, err = h.renderPanel(ctx, c, e, p)
		if err != nil {
			return 0, err
		}
	}
	// Planning and nonce creation may race a recovery transition. Fence again
	// before touching Telegram, not just before acknowledging the notice.
	latestConnection, latestEpisode, err := h.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	if !progressFenceMatches(latestConnection, latestEpisode, e) || latestEpisode.StatusMessageID != e.StatusMessageID {
		return 0, storage.ErrRecoverySuperseded
	}
	id := e.StatusMessageID
	if id > 0 {
		err = h.Client.EditText(ctx, h.ChatID, id, p.text, markup)
		if err != nil {
			var transport *TransportError
			if !errors.As(err, &transport) || transport.Code != "TELEGRAM_MESSAGE_UNEDITABLE" {
				return 0, err
			}
			id = 0
			// A replacement message needs fresh message-bound controls.
			if len(actions) == 0 {
				markup, actions, err = h.renderPanel(ctx, c, e, p)
				if err != nil {
					return 0, err
				}
			}
		}
	}
	created := id == 0
	if id == 0 {
		latestConnection, latestEpisode, err = h.snapshot(ctx)
		if err != nil {
			return 0, err
		}
		if !progressFenceMatches(latestConnection, latestEpisode, e) || latestEpisode.StatusMessageID != e.StatusMessageID {
			return 0, storage.ErrRecoverySuperseded
		}
		id, err = h.Client.SendText(ctx, h.ChatID, p.text, markup)
		if err != nil {
			return 0, err
		}
		if err := h.Store.SetAuthRecoveryStatusMessage(ctx, e.ID, c.Generation, id); err != nil {
			_ = h.Client.DeleteMessage(ctx, h.ChatID, id)
			return 0, err
		}
	}
	if err := h.bindActions(ctx, actions, id); err != nil {
		return 0, err
	}
	latestConnection, latestEpisode, err = h.snapshot(ctx)
	if err == nil && (!progressFenceMatches(latestConnection, latestEpisode, e) || latestEpisode.StatusMessageID != id) {
		err = storage.ErrRecoverySuperseded
	}
	if err != nil {
		// Never delete the pre-existing canonical panel on a stale edit.
		if created {
			_ = h.Client.DeleteMessage(ctx, h.ChatID, id)
		}
		return 0, err
	}
	h.progressEpisode, h.progressText, h.progressKey, h.progressMarkup = e.ID, p.text, key, markup
	return id, nil
}
func (h *Handler) deliverLogoutNotices(ctx context.Context) error {
	jobs, err := h.Store.PendingACBLogoutNotices(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		local := "• Hệ thống: Đang chờ xóa phiên..."
		if job.LocalClearedAt != "" {
			local = "• Hệ thống: ✅ Đã xóa phiên lưu trữ trên máy chủ."
		}
		bank := "• Ngân hàng: Đang xử lý thu hồi phiên..."
		switch job.BankStatus {
		case "CONFIRMED":
			bank = "• Ngân hàng: ✅ ACB đã xác nhận thu hồi phiên an toàn."
		case "ALREADY_EXPIRED":
			bank = "• Ngân hàng: ℹ️ Phiên làm việc đã hết hạn tại ACB từ trước."
		case "UNCONFIRMED":
			bank = "• Ngân hàng: ⚠️ Chưa nhận được xác nhận từ ACB. Bạn có thể kiểm tra thêm trên app ACB ONE."
		case "IN_FLIGHT":
			bank = "• Ngân hàng: 🔄 Đang gửi lệnh thu hồi phiên tới ACB..."
		}
		text := "🚪 KẾT QUẢ ĐĂNG XUẤT ACB\n────────────────────────\n" + local + "\n" + bank + "\n\n💡 Hệ thống đã đưa về trạng thái chờ. Bấm 'Đăng nhập' khi muốn tạo phiên mới."
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
