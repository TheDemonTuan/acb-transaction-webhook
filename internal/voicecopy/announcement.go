package voicecopy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const DefaultAnnouncementTemplate = "Đa tạ quý khách vì {amount}."

var urlPattern = regexp.MustCompile(`https?://\S+`)
var punctRegex = regexp.MustCompile(`\s+([.,;:!?])`)
var multiSpaceRegex = regexp.MustCompile(`\s+`)
var multiDotRegex = regexp.MustCompile(`\.+`)
var descCleanupRegex = regexp.MustCompile(`[,;]?\s*(?:Nội dung|nội dung):\s*\{description\}`)

// SanitizeDescription cleanses transaction description for TTS synthesis:
// strips URLs, control characters, special characters, and limits length to maxLength runes.
func SanitizeDescription(desc string, maxLength int) string {
	if desc == "" {
		return ""
	}
	if maxLength <= 0 {
		maxLength = 80
	}

	// Remove URLs
	s := urlPattern.ReplaceAllString(desc, " ")

	// Filter characters: letters, numbers, spaces, and simple punctuation (,. -)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || r == ',' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}

	// Collapse multiple spaces
	words := strings.Fields(b.String())
	cleaned := strings.Join(words, " ")

	runes := []rune(cleaned)
	if len(runes) > maxLength {
		runes = runes[:maxLength]
		cleaned = strings.TrimSpace(string(runes))
	}

	return cleaned
}

// FormatAnnouncementTemplate formats an announcement phrase from a template with token replacement.
// Supported tokens:
// - {amount}: Spoken Vietnamese words for the amount (via SpeakVND)
// - {amount_raw}: Unformatted/raw numeric amount string
// - {description}: Sanitized transaction description
func FormatAnnouncementTemplate(template string, amount int64, description string, includeDescription bool) string {
	tpl := strings.TrimSpace(template)
	if tpl == "" {
		tpl = DefaultAnnouncementTemplate
	}

	spokenAmount := SpeakVND(amount)
	rawAmount := strconv.FormatInt(amount, 10)
	sanitizedDesc := SanitizeDescription(description, 80)

	result := tpl
	result = strings.ReplaceAll(result, "{amount}", spokenAmount)
	result = strings.ReplaceAll(result, "{amount_raw}", rawAmount)

	if strings.Contains(result, "{description}") {
		hasValidDesc := includeDescription && sanitizedDesc != ""
		if !hasValidDesc {
			result = descCleanupRegex.ReplaceAllString(result, "")
			result = strings.ReplaceAll(result, "{description}", "")
		} else {
			result = strings.ReplaceAll(result, "{description}", sanitizedDesc)
		}
	} else if includeDescription && sanitizedDesc != "" {
		trimmed := strings.TrimSpace(result)
		if strings.HasSuffix(trimmed, ".") {
			result = trimmed + " Nội dung: " + sanitizedDesc + "."
		} else {
			result = trimmed + ". Nội dung: " + sanitizedDesc + "."
		}
	}

	result = multiSpaceRegex.ReplaceAllString(result, " ")
	result = punctRegex.ReplaceAllString(result, "$1")
	result = multiDotRegex.ReplaceAllString(result, ".")
	return strings.TrimSpace(result)
}

// BuildCreditAnnouncement creates a natural Vietnamese announcement phrase for an incoming transaction using the default template.
func BuildCreditAnnouncement(amount int64, description string, includeDescription bool) string {
	return FormatAnnouncementTemplate(DefaultAnnouncementTemplate, amount, description, includeDescription)
}

// BuildBurstAnnouncement creates an aggregate announcement phrase for multiple transactions.
func BuildBurstAnnouncement(count int, totalAmount int64) string {
	totalWords := SpeakVND(totalAmount)
	return fmt.Sprintf("Bạn vừa nhận được %d giao dịch mới, tổng cộng %s.", count, totalWords)
}
