package sepay

import (
	"bytes"
	"encoding/json"
	"errors"
)

// TelegramUpdate keeps the original HTTP body for encrypted, durable evidence.
// Unknown update kinds are ignored, rather than persisted with their private data.
type TelegramUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *TelegramMessage `json:"message"`
	EditedMessage *TelegramMessage `json:"edited_message"`
	RawPayload    json.RawMessage  `json:"-"`
	unsupported   bool
}

type TelegramUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type TelegramChat struct {
	ID int64 `json:"id"`
}

// Only an original text message is financial evidence. Rich text entities do
// not alter Text; forwarded, replied-to, channel, media and service messages
// fail the message-field allowlist, even if they also contain a text field.
type TelegramMessage struct {
	MessageID       int64         `json:"message_id"`
	Date            int64         `json:"date"`
	From            *TelegramUser `json:"from"`
	Chat            TelegramChat  `json:"chat"`
	MessageThreadID int64         `json:"message_thread_id"`
	Text            string        `json:"text"`
	EditDate        int64         `json:"edit_date,omitempty"`
	unsupported     bool
}

func (m *TelegramMessage) UnmarshalJSON(raw []byte) error {
	type messageFields TelegramMessage
	var decoded messageFields
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("telegram message must be an object")
	}
	for field := range fields {
		switch field {
		case "message_id", "date", "from", "chat", "message_thread_id", "is_topic_message", "text", "entities", "edit_date":
		default:
			decoded.unsupported = true
		}
	}
	*m = TelegramMessage(decoded)
	return nil
}

func (u *TelegramUpdate) UnmarshalJSON(raw []byte) error {
	type updateFields TelegramUpdate
	var decoded updateFields
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("telegram update must be an object")
	}
	for field := range fields {
		switch field {
		case "update_id", "message", "edited_message":
		default:
			decoded.unsupported = true
		}
	}
	if id, present := fields["update_id"]; !present {
		decoded.unsupported = true
	} else if bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		return errors.New("telegram update_id must be an integer")
	}
	*u = TelegramUpdate(decoded)
	return nil
}

// DecodeTelegramUpdate follows the HTTP layer's strict duplicate-key/depth
// validation. IDs decode directly to int64, never through float64. RawPayload
// is retained verbatim; storage owns canonical hashing and encryption.
func DecodeTelegramUpdate(raw []byte) (TelegramUpdate, error) {
	var update TelegramUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		return TelegramUpdate{}, errors.New("invalid telegram update")
	}
	update.RawPayload = raw
	return update, nil
}

func (u TelegramUpdate) trustedMessage(cfg Config) (*TelegramMessage, bool) {
	if u.unsupported || u.UpdateID < 0 || (u.Message != nil && u.EditedMessage != nil) {
		return nil, false
	}
	message, edited := u.Message, false
	if message == nil {
		message, edited = u.EditedMessage, true
	}
	if message == nil || message.unsupported || message.MessageID <= 0 || message.Date <= 0 || message.Text == "" {
		return nil, false
	}
	if message.From == nil || message.From.ID != cfg.SenderBotID || !message.From.IsBot || message.Chat.ID != cfg.ChatID || message.MessageThreadID != cfg.TopicID {
		return nil, false
	}
	if !edited && message.EditDate != 0 {
		return nil, false
	}
	return message, edited
}
