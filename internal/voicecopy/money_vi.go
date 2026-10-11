package voicecopy

import (
	"strings"
)

var digits = []string{"không", "một", "hai", "ba", "bốn", "năm", "sáu", "bảy", "tám", "chín"}

func readThreeDigits(n int, hasHigherGroup bool) []string {
	hundreds := n / 100
	tens := (n % 100) / 10
	units := n % 10
	var words []string

	if hundreds > 0 || hasHigherGroup {
		words = append(words, digits[hundreds], "trăm")
	}

	if tens > 1 {
		words = append(words, digits[tens], "mươi")
		switch units {
		case 1:
			words = append(words, "mốt")
		case 4:
			words = append(words, "tư")
		case 5:
			words = append(words, "lăm")
		default:
			if units > 0 {
				words = append(words, digits[units])
			}
		}
	} else if tens == 1 {
		words = append(words, "mười")
		if units == 5 {
			words = append(words, "lăm")
		} else if units > 0 {
			words = append(words, digits[units])
		}
	} else if tens == 0 {
		if units > 0 {
			if hundreds > 0 || hasHigherGroup {
				words = append(words, "linh", digits[units])
			} else {
				words = append(words, digits[units])
			}
		}
	}

	return words
}

// SpeakVND converts an int64 amount into natural spoken Vietnamese text followed by "đồng".
// Example: 500000 -> "năm trăm nghìn đồng"
// Example: 1250000 -> "một triệu hai trăm năm mươi nghìn đồng"
func SpeakVND(amount int64) string {
	if amount <= 0 {
		return "không đồng"
	}
	// Break amount into groups of 3 digits from right to left
	var groups []int
	val := amount
	for val > 0 {
		groups = append(groups, int(val%1000))
		val /= 1000
	}

	// Pad groups to a multiple of 3 so each block has 3 groups (đơn vị, nghìn, triệu)
	for len(groups)%3 != 0 {
		groups = append(groups, 0)
	}

	numBlocks := len(groups) / 3
	var resultParts []string
	hasHigherOverall := false

	for b := numBlocks - 1; b >= 0; b-- {
		g2 := groups[b*3+2] // triệu
		g1 := groups[b*3+1] // nghìn
		g0 := groups[b*3+0] // đơn vị

		blockHasVal := (g2 > 0 || g1 > 0 || g0 > 0)
		if !blockHasVal {
			continue
		}

		if g2 > 0 {
			w := readThreeDigits(g2, hasHigherOverall)
			resultParts = append(resultParts, w...)
			resultParts = append(resultParts, "triệu")
			hasHigherOverall = true
		}

		if g1 > 0 {
			w := readThreeDigits(g1, hasHigherOverall)
			resultParts = append(resultParts, w...)
			resultParts = append(resultParts, "nghìn")
			hasHigherOverall = true
		}

		if g0 > 0 {
			w := readThreeDigits(g0, hasHigherOverall)
			resultParts = append(resultParts, w...)
			hasHigherOverall = true
		}

		if b > 0 {
			for range b {
				resultParts = append(resultParts, "tỷ")
			}
		}
	}

	if len(resultParts) == 0 {
		return "không đồng"
	}

	return strings.Join(resultParts, " ") + " đồng"
}
