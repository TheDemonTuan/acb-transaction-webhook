package telegramauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

// BaseURL is an explicit dependency injection point for local harnesses, not an
// environment option. Production callers leave it empty.
type ClientOptions struct {
	HTTPClient *http.Client
	BaseURL    string
}
type TransportState struct {
	BotID      int64
	Ready      bool
	Stopped    bool
	ReasonCode string
}
type Client struct {
	token, base string
	http        *http.Client
	mu          sync.RWMutex
	state       TransportState
	polling     bool
}
type TransportError struct {
	Code       string
	RetryAfter time.Duration
	Fatal      bool
}

func (e *TransportError) Error() string             { return e.Code }
func (e *TransportError) RetryDelay() time.Duration { return e.RetryAfter }
func NewClient(token string, options ClientOptions) (*Client, error) {
	if token == "" {
		return nil, errors.New("TELEGRAM_TOKEN_REQUIRED")
	}
	for _, r := range token {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ':' || r == '_' || r == '-') {
			return nil, errors.New("TELEGRAM_TOKEN_INVALID")
		}
	}
	base := options.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("TELEGRAM_ENDPOINT_INVALID")
	}
	h := options.HTTPClient
	if h == nil {
		h = &http.Client{}
	}
	// Never follow redirects: a redirected Bot API URL would disclose the token.
	clone := *h
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{token: token, base: strings.TrimRight(base, "/"), http: &clone}, nil
}
func (c *Client) Readiness() TransportState { c.mu.RLock(); defer c.mu.RUnlock(); return c.state }
func (c *Client) setTransport(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		if !c.state.Stopped && c.state.BotID != 0 {
			c.state.Ready = true
			c.state.ReasonCode = ""
		}
		return
	}
	c.state.Ready = false
	c.state.ReasonCode = "TELEGRAM_UNAVAILABLE"
	var e *TransportError
	if errors.As(err, &e) {
		c.state.ReasonCode = e.Code
		if e.Fatal {
			c.state.Stopped = true
		}
	}
}
func (c *Client) request(ctx context.Context, method, contentType string, body io.Reader, out any) error {
	if c.Readiness().Stopped {
		return &TransportError{Code: "TELEGRAM_TRANSPORT_STOPPED", Fatal: true}
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, body)
	if err != nil {
		return &TransportError{Code: "TELEGRAM_REQUEST_FAILED"}
	}
	req.Header.Set("Content-Type", contentType)
	res, err := c.http.Do(req)
	if err != nil {
		e := &TransportError{Code: "TELEGRAM_NETWORK_ERROR"}
		c.setTransport(e)
		return e
	}
	defer res.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(res.Body, 1024*1024+1))
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	decodeErr := json.Unmarshal(data, &envelope)
	code := res.StatusCode
	if envelope.ErrorCode != 0 {
		code = envelope.ErrorCode
	}
	var failure *TransportError
	switch {
	case code == 401 || code == 403:
		failure = &TransportError{Code: "TELEGRAM_AUTH_REJECTED", Fatal: true}
	case code == 429:
		seconds := envelope.Parameters.RetryAfter
		if seconds < 1 {
			seconds = 1
		}
		if seconds > 300 {
			seconds = 300
		}
		failure = &TransportError{Code: "TELEGRAM_RATE_LIMITED", RetryAfter: time.Duration(seconds) * time.Second}
	case readErr != nil || len(data) > 1024*1024 || decodeErr != nil:
		failure = &TransportError{Code: "TELEGRAM_INVALID_RESPONSE"}
	case method == "deleteMessage" && code == 400 && envelope.Description == "Bad Request: message to delete not found":
		c.setTransport(nil)
		return nil
	case method == "editMessageText" && code == 400 && envelope.Description == "Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message":
		failure = &TransportError{Code: "TELEGRAM_MESSAGE_NOT_MODIFIED"}
	case method == "editMessageText" && code == 400 && envelope.Description == "Bad Request: message is not modified":
		failure = &TransportError{Code: "TELEGRAM_MESSAGE_NOT_MODIFIED"}
	case method == "editMessageText" && code == 400 && (envelope.Description == "Bad Request: message can't be edited" || envelope.Description == "Bad Request: message to edit not found"):
		failure = &TransportError{Code: "TELEGRAM_MESSAGE_UNEDITABLE"}
	case res.StatusCode < 200 || res.StatusCode >= 300 || !envelope.OK:
		failure = &TransportError{Code: "TELEGRAM_API_ERROR"}
	}
	if failure != nil {
		c.setTransport(failure)
		return failure
	}
	if out != nil && json.Unmarshal(envelope.Result, out) != nil {
		failure = &TransportError{Code: "TELEGRAM_INVALID_RESPONSE"}
		c.setTransport(failure)
		return failure
	}
	c.setTransport(nil)
	return nil
}
func (c *Client) call(ctx context.Context, method string, input, out any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return errors.New("TELEGRAM_REQUEST_FAILED")
	}
	return c.request(ctx, method, "application/json", bytes.NewReader(data), out)
}
func (c *Client) CheckConfig(ctx context.Context) (int64, error) {
	var me struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	}
	if err := c.call(ctx, "getMe", struct{}{}, &me); err != nil {
		return 0, err
	}
	if me.ID == 0 || !me.IsBot {
		e := &TransportError{Code: "TELEGRAM_IDENTITY_INVALID", Fatal: true}
		c.setTransport(e)
		return 0, e
	}
	var webhook struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, "getWebhookInfo", struct{}{}, &webhook); err != nil {
		return 0, err
	}
	if webhook.URL != "" {
		e := &TransportError{Code: "TELEGRAM_WEBHOOK_CONFLICT", Fatal: true}
		c.setTransport(e)
		return 0, e
	}
	c.mu.Lock()
	c.state.BotID = me.ID
	c.state.Ready = true
	c.mu.Unlock()
	return me.ID, nil
}

// CheckOperator validates the configured private destination without sending a
// challenge, enrolling users, or consuming updates.
func (c *Client) CheckOperator(ctx context.Context, chatID, userID int64) error {
	if chatID == 0 || userID == 0 {
		return errors.New("TELEGRAM_OPERATOR_INVALID")
	}
	var chat Chat
	if err := c.call(ctx, "getChat", map[string]int64{"chat_id": chatID}, &chat); err != nil {
		return err
	}
	if chat.ID != chatID || chat.Type != "private" {
		return errors.New("TELEGRAM_PRIVATE_CHAT_REQUIRED")
	}
	return nil
}

// SetCommands scopes command discovery to the configured operator chat only.
func (c *Client) SetCommands(ctx context.Context, chatID int64) error {
	if chatID == 0 {
		return errors.New("TELEGRAM_OPERATOR_INVALID")
	}
	return c.call(ctx, "setMyCommands", map[string]any{
		"scope": map[string]any{"type": "chat", "chat_id": chatID},
		"commands": []map[string]string{
			{"command": "menu", "description": "Bảng điều khiển ACB"},
			{"command": "acb_status", "description": "Trạng thái phiên ACB"},
			{"command": "help", "description": "Hướng dẫn sử dụng"},
		},
	}, nil)
}

func (c *Client) EditText(ctx context.Context, chatID, messageID int64, text string, markup any) error {
	err := c.call(ctx, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": text, "reply_markup": markup}, nil)
	var transport *TransportError
	if errors.As(err, &transport) && transport.Code == "TELEGRAM_MESSAGE_NOT_MODIFIED" {
		c.setTransport(nil)
		return nil
	}
	return err
}
func (c *Client) SendText(ctx context.Context, chatID int64, text string, markup any) (int64, error) {
	var result struct {
		ID int64 `json:"message_id"`
	}
	err := c.call(ctx, "sendMessage", struct {
		ChatID      int64           `json:"chat_id"`
		Text        string          `json:"text"`
		Markup      any             `json:"reply_markup,omitempty"`
		Protect     bool            `json:"protect_content"`
		LinkPreview map[string]bool `json:"link_preview_options"`
	}{chatID, text, markup, true, map[string]bool{"is_disabled": true}}, &result)
	if err == nil && result.ID <= 0 {
		err = errors.New("TELEGRAM_INVALID_MESSAGE")
	}
	return result.ID, err
}
func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "deleteMessage", map[string]int64{"chat_id": chatID, "message_id": messageID}, nil)
}
func (c *Client) AnswerCallbackQuery(ctx context.Context, id string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": id}, nil)
}
func localExpiry(value string) string {
	expiry, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "không xác định"
	}
	return expiry.In(time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)).Format("15:04:05 02/01/2006")
}
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
func (c *Client) SendChallenge(ctx context.Context, ch storage.AuthChallenge, image []byte) (int64, error) {
	force := map[string]any{"force_reply": true, "selective": true, "input_field_placeholder": "Trả lời trực tiếp yêu cầu này"}
	suffix := fmt.Sprintf(" Mã yêu cầu: %s. Hết hạn: %s.", shortID(ch.ID), localExpiry(ch.ExpiresAt))
	if ch.Kind == "OTP" {
		return c.SendText(ctx, ch.ChatID, "ACB cần OTP đăng nhập. Chỉ nhập mã do ACB cấp cho lần đăng nhập này, không phải OTP chuyển tiền. Trả lời trực tiếp tin này."+suffix, force)
	}
	if ch.Kind != "CAPTCHA_TEXT" {
		return 0, errors.New("TELEGRAM_CAPTCHA_INVALID")
	}
	return c.sendCaptchaPhoto(ctx, ch.ChatID, image, "ACB cần CAPTCHA. Trả lời trực tiếp ảnh này bằng ký tự trong ảnh."+suffix, force)
}
func (c *Client) SendCaptchaImage(ctx context.Context, chatID int64, image []byte) (int64, error) {
	return c.sendCaptchaPhoto(ctx, chatID, image, "Ảnh captcha hiện hành. Ảnh này không tạo yêu cầu trả lời mới.", nil)
}
func (c *Client) sendCaptchaPhoto(ctx context.Context, chatID int64, image []byte, caption string, markup any) (int64, error) {
	if len(image) > 512*1024 || len(image) < 8 || !bytes.Equal(image[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return 0, errors.New("TELEGRAM_CAPTCHA_INVALID")
	}
	config, err := png.DecodeConfig(bytes.NewReader(image))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 4096 || config.Height > 4096 {
		return 0, errors.New("TELEGRAM_CAPTCHA_INVALID")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10), "caption": caption, "protect_content": "true"}
	if markup != nil {
		encoded, _ := json.Marshal(markup)
		fields["reply_markup"] = string(encoded)
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return 0, errors.New("TELEGRAM_REQUEST_FAILED")
		}
	}
	part, err := writer.CreateFormFile("photo", "captcha.png")
	if err != nil {
		return 0, errors.New("TELEGRAM_REQUEST_FAILED")
	}
	if _, err = part.Write(image); err != nil {
		return 0, errors.New("TELEGRAM_REQUEST_FAILED")
	}
	if err = writer.Close(); err != nil {
		return 0, errors.New("TELEGRAM_REQUEST_FAILED")
	}
	defer clear(body.Bytes())
	var result struct {
		ID int64 `json:"message_id"`
	}
	err = c.request(ctx, "sendPhoto", writer.FormDataContentType(), &body, &result)
	if err == nil && result.ID <= 0 {
		err = errors.New("TELEGRAM_INVALID_MESSAGE")
	}
	return result.ID, err
}

type User struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type Message struct {
	ID                int64           `json:"message_id"`
	Chat              Chat            `json:"chat"`
	From              *User           `json:"from"`
	Text              string          `json:"text"`
	ReplyTo           *Message        `json:"reply_to_message"`
	ForwardOrigin     json.RawMessage `json:"forward_origin"`
	ForwardFrom       json.RawMessage `json:"forward_from"`
	ForwardFromChat   json.RawMessage `json:"forward_from_chat"`
	ForwardDate       int64           `json:"forward_date"`
	ViaBot            json.RawMessage `json:"via_bot"`
	SenderChat        json.RawMessage `json:"sender_chat"`
	EditDate          int64           `json:"edit_date"`
	Photo             json.RawMessage `json:"photo"`
	Video             json.RawMessage `json:"video"`
	Animation         json.RawMessage `json:"animation"`
	Audio             json.RawMessage `json:"audio"`
	Voice             json.RawMessage `json:"voice"`
	VideoNote         json.RawMessage `json:"video_note"`
	Document          json.RawMessage `json:"document"`
	Contact           json.RawMessage `json:"contact"`
	Sticker           json.RawMessage `json:"sticker"`
	Location          json.RawMessage `json:"location"`
	Venue             json.RawMessage `json:"venue"`
	Poll              json.RawMessage `json:"poll"`
	Dice              json.RawMessage `json:"dice"`
	Story             json.RawMessage `json:"story"`
	PaidMedia         json.RawMessage `json:"paid_media"`
	Game              json.RawMessage `json:"game"`
	Invoice           json.RawMessage `json:"invoice"`
	SuccessfulPayment json.RawMessage `json:"successful_payment"`
	PassportData      json.RawMessage `json:"passport_data"`
	WebAppData        json.RawMessage `json:"web_app_data"`
}
type CallbackQuery struct {
	ID              string   `json:"id"`
	From            User     `json:"from"`
	Message         *Message `json:"message"`
	Data            string   `json:"data"`
	InlineMessageID string   `json:"inline_message_id"`
}
type Update struct {
	ID            int64           `json:"update_id"`
	Message       *Message        `json:"message"`
	Callback      *CallbackQuery  `json:"callback_query"`
	EditedMessage json.RawMessage `json:"edited_message"`
}
type UpdateHandler interface {
	HandleUpdate(context.Context, Update) error
}

func retryDelay(err error, failures int) time.Duration {
	var e *TransportError
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter
	}
	if failures > 7 {
		failures = 7
	}
	if failures < 1 {
		failures = 1
	}
	seconds := 1 << (failures - 1)
	delay := time.Duration(seconds) * time.Second
	delay += time.Duration(rand.Float64() * float64(delay) / 5)
	if delay > 60*time.Second {
		delay = 60 * time.Second
	}
	return delay
}
func waitRetry(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// PollOnce commits offsets only after the handler's durable disposition. Replays
// are safe because challenge/action consumption is fenced in storage.
func (c *Client) pollOnce(ctx context.Context, store *storage.Store, handler UpdateHandler) error {
	state, err := store.TelegramAuthState(ctx, c.Readiness().BotID)
	if err != nil {
		return err
	}
	var updates []Update
	err = c.call(ctx, "getUpdates", map[string]any{"offset": state.NextUpdateID, "timeout": 30, "limit": 100, "allowed_updates": []string{"message", "callback_query"}}, &updates)
	if err != nil {
		return err
	}
	for _, u := range updates {
		if u.ID < state.NextUpdateID {
			continue
		}
		if u.ID < 0 || u.ID == int64(^uint64(0)>>1) {
			return errors.New("TELEGRAM_UPDATE_INVALID")
		}
		if err := handler.HandleUpdate(ctx, u); err != nil {
			return err
		}
		if err := store.AdvanceTelegramAuthOffset(ctx, state.BotID, u.ID+1); err != nil {
			return err
		}
		state.NextUpdateID = u.ID + 1
	}
	return nil
}
func (c *Client) RunPolling(ctx context.Context, store *storage.Store, handler UpdateHandler, chatID int64) error {
	if store == nil || handler == nil || chatID == 0 {
		return errors.New("TELEGRAM_POLLING_DEPENDENCY_REQUIRED")
	}
	c.mu.Lock()
	if c.polling {
		c.mu.Unlock()
		return errors.New("TELEGRAM_POLLING_ALREADY_RUNNING")
	}
	c.polling = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.polling = false; c.mu.Unlock() }()
	failures := 0
	commandsRegistered := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var err error
		if c.Readiness().BotID == 0 {
			_, err = c.CheckConfig(ctx)
		}
		if err == nil && !commandsRegistered {
			err = c.SetCommands(ctx, chatID)
			commandsRegistered = err == nil
		}
		if err == nil {
			err = c.pollOnce(ctx, store, handler)
		}
		if err == nil {
			failures = 0
			continue
		}
		var e *TransportError
		if errors.As(err, &e) && e.Fatal {
			return e
		}
		failures++
		if err := waitRetry(ctx, retryDelay(err, failures)); err != nil {
			return err
		}
	}
}
