package acb

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

type Transaction struct {
	Number        string
	EffectiveDate string
	TransactionAt string
	Debit         int64
	Credit        int64
	Balance       *int64
	Description   string
}

type HistoryPageResult struct {
	Transactions []Transaction
	HasNext      bool
	NextAction   string
	NextFields   map[string]string
	TotalRows    int
}

// ParseHistoryPage parses an ACB history page, returning transactions and pagination signals.
func ParseHistoryPage(markup string) (HistoryPageResult, error) {
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		return HistoryPageResult{}, err
	}
	var tables []*html.Node
	walk(doc, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "table" {
			tables = append(tables, node)
		}
	})
	var recognized bool
	var emptyHistory bool
	var parsedTransactions []Transaction

	for _, table := range tables {
		rows := tableRows(table)
		headerIndex, columns := historyHeader(rows)
		if headerIndex < 0 {
			continue
		}
		recognized = true
		transactions := make([]Transaction, 0, len(rows)-headerIndex-1)
		for i := headerIndex + 1; i < len(rows); i++ {
			row := rows[i]
			if len(row) == 0 || allBlank(row) {
				continue
			}
			transaction, err := parseRow(row, columns)
			if err != nil {
				if len(transactions) > 0 {
					var nonBlank []string
					for _, cell := range row {
						t := strings.TrimSpace(cell)
						if t != "" {
							nonBlank = append(nonBlank, t)
						}
					}
					if len(nonBlank) > 0 && transactions[len(transactions)-1].Description == "" {
						transactions[len(transactions)-1].Description = strings.Join(nonBlank, " ")
						continue
					}
				}
				rowText := strings.ToLower(strings.Join(row, " "))
				if containsAny(rowText, "khong co giao dich", "không có giao dịch", "khong co du lieu", "không có dữ liệu", "no transaction", "chua co giao dich", "chưa có giao dịch") {
					emptyHistory = true
					continue
				}
				if isTableFooter(rowText) {
					continue
				}
				return HistoryPageResult{}, err
			}
			if columns.description < 0 && i+1 < len(rows) {
				nextRow := rows[i+1]
				if len(nextRow) > 0 && !allBlank(nextRow) {
					if _, errNext := parseRow(nextRow, columns); errNext != nil {
						var nonBlank []string
						for _, cell := range nextRow {
							t := strings.TrimSpace(cell)
							if t != "" {
								nonBlank = append(nonBlank, t)
							}
						}
						if len(nonBlank) > 0 {
							transaction.Description = strings.Join(nonBlank, " ")
							i++
						}
					}
				}
			}
			transactions = append(transactions, transaction)
		}
		if len(transactions) > 0 {
			parsedTransactions = transactions
			break
		}
	}

	if len(parsedTransactions) == 0 {
		if recognized && emptyHistory {
			parsedTransactions = []Transaction{}
		} else if recognized {
			return HistoryPageResult{}, errors.New("ACB history table has no transactions and no recognized empty-state marker")
		} else {
			return HistoryPageResult{}, errors.New("ACB history table schema not recognized")
		}
	}

	hasNext, nextAction, nextFields, totalRows := detectPagination(doc, markup)
	return HistoryPageResult{
		Transactions: parsedTransactions,
		HasNext:      hasNext,
		NextAction:   nextAction,
		NextFields:   nextFields,
		TotalRows:    totalRows,
	}, nil
}

// ParseHistory requires a recognizable table header and rejects malformed rows;
// callers must quarantine rejected pages rather than treating them as empty.
func ParseHistory(markup string) ([]Transaction, error) {
	page, err := ParseHistoryPage(markup)
	return page.Transactions, err
}

func historyHeader(rows [][]string) (int, columns) {
	for i, row := range rows {
		indexes := columnIndexes(row)
		if indexes.valid() {
			return i, indexes
		}
	}
	return -1, columns{}
}

func isTableFooter(text string) bool {
	return containsAny(text,
		"trang truoc", "trang trước", "trang sau", "next", "previous",
		"xuat excel", "xuất excel",
	)
}

type columns struct{ number, effective, transaction, debit, credit, balance, description int }

func (c columns) valid() bool {
	return c.number >= 0 && c.transaction >= 0 && c.debit >= 0 && c.credit >= 0
}

func columnIndexes(header []string) columns {
	indexes := columns{number: -1, effective: -1, transaction: -1, debit: -1, credit: -1, balance: -1, description: -1}
	for index, name := range header {
		switch normalized(name) {
		case "sogd", "sogiaodich", "sogiao dich", "transactionnumber":
			indexes.number = index
		case "ngayhieuluc", "effective date":
			indexes.effective = index
		case "ngaygiaodich", "transaction date":
			indexes.transaction = index
		case "ghino", "debit":
			indexes.debit = index
		case "ghico", "credit":
			indexes.credit = index
		case "sodu", "balance":
			indexes.balance = index
		case "noidunggiaodich", "description", "content":
			indexes.description = index
		}
	}
	return indexes
}

func parseRow(row []string, c columns) (Transaction, error) {
	get := func(index int) string {
		if index < 0 || index >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[index])
	}
	debit, err := parseMoney(get(c.debit))
	if err != nil {
		return Transaction{}, fmt.Errorf("invalid debit: %w", err)
	}
	credit, err := parseMoney(get(c.credit))
	if err != nil {
		return Transaction{}, fmt.Errorf("invalid credit: %w", err)
	}
	if get(c.number) == "" || get(c.transaction) == "" {
		return Transaction{}, errors.New("transaction row is missing transaction number or date")
	}
	transaction := Transaction{Number: get(c.number), EffectiveDate: get(c.effective), TransactionAt: get(c.transaction), Debit: debit, Credit: credit, Description: get(c.description)}
	if raw := get(c.balance); raw != "" {
		balance, err := parseMoney(raw)
		if err != nil {
			return Transaction{}, fmt.Errorf("invalid balance: %w", err)
		}
		transaction.Balance = &balance
	}
	return transaction, nil
}

func parseMoney(value string) (int64, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, " ", ""))
	if value == "" || value == "-" {
		return 0, nil
	}
	negative := strings.HasPrefix(value, "(") && strings.HasSuffix(value, ")")
	value = strings.Trim(value, "()")
	var digits strings.Builder
	for _, r := range value {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		} else if !strings.ContainsRune("., ₫VNDvnd", r) {
			return 0, errors.New("unexpected currency character")
		}
	}
	if digits.Len() == 0 {
		return 0, errors.New("missing amount")
	}
	amount, err := strconv.ParseInt(digits.String(), 10, 64)
	if err != nil {
		return 0, err
	}
	if negative {
		return -amount, nil
	}
	return amount, nil
}

func tableRows(table *html.Node) [][]string {
	var rows [][]string
	appendRow := func(tr *html.Node) {
		var cells []string
		for child := tr.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && (child.Data == "th" || child.Data == "td") {
				cells = append(cells, strings.TrimSpace(text(child)))
			}
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
	}

	for child := table.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && child.Data == "tr" {
			appendRow(child)
		} else if child.Type == html.ElementNode && (child.Data == "tbody" || child.Data == "thead" || child.Data == "tfoot") {
			for subChild := child.FirstChild; subChild != nil; subChild = subChild.NextSibling {
				if subChild.Type == html.ElementNode && subChild.Data == "tr" {
					appendRow(subChild)
				}
			}
		}
	}
	return rows
}

func walk(node *html.Node, visit func(*html.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func text(node *html.Node) string {
	var values []string
	walk(node, func(current *html.Node) {
		if current.Type == html.TextNode {
			values = append(values, current.Data)
		}
	})
	return strings.Join(values, " ")
}

func allBlank(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func normalized(value string) string {
	value = strings.NewReplacer(
		"à", "a", "á", "a", "ả", "a", "ã", "a", "ạ", "a", "ă", "a", "ằ", "a", "ắ", "a", "ẳ", "a", "ẵ", "a", "ặ", "a", "â", "a", "ầ", "a", "ấ", "a", "ẩ", "a", "ẫ", "a", "ậ", "a",
		"è", "e", "é", "e", "ẻ", "e", "ẽ", "e", "ẹ", "e", "ê", "e", "ề", "e", "ế", "e", "ể", "e", "ễ", "e", "ệ", "e",
		"ì", "i", "í", "i", "ỉ", "i", "ĩ", "i", "ị", "i",
		"ò", "o", "ó", "o", "ỏ", "o", "õ", "o", "ọ", "o", "ô", "o", "ồ", "o", "ố", "o", "ổ", "o", "ỗ", "o", "ộ", "o", "ơ", "o", "ờ", "o", "ớ", "o", "ở", "o", "ỡ", "o", "ợ", "o",
		"ù", "u", "ú", "u", "ủ", "u", "ũ", "u", "ụ", "u", "ư", "u", "ừ", "u", "ứ", "u", "ử", "u", "ữ", "u", "ự", "u",
		"ỳ", "y", "ý", "y", "ỷ", "y", "ỹ", "y", "ỵ", "y", "đ", "d",
	).Replace(strings.ToLower(strings.TrimSpace(value)))
	var out strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			out.WriteRune(r)
		}
	}
	return strings.ReplaceAll(out.String(), " ", "")
}

func detectPagination(doc *html.Node, markup string) (hasNext bool, nextAction string, nextFields map[string]string, totalRows int) {
	var docTextBuilder strings.Builder
	var nextCandidates []*html.Node
	var nextDisabled bool

	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.TextNode {
			docTextBuilder.WriteString(n.Data)
			docTextBuilder.WriteString(" ")
		}
		if n.Type == html.ElementNode {
			name := strings.ToLower(strings.TrimSpace(attrVal(n, "name")))
			id := strings.ToLower(strings.TrimSpace(attrVal(n, "id")))
			if name == "next_disabled" || id == "next_disabled" {
				nextDisabled = true
			}

			tag := strings.ToLower(n.Data)
			if tag == "a" || tag == "button" || tag == "input" {
				t := strings.ToLower(strings.TrimSpace(nodeText(n)))
				v := strings.ToLower(strings.TrimSpace(attrVal(n, "value")))
				class := strings.ToLower(strings.TrimSpace(attrVal(n, "class")))
				rel := strings.ToLower(strings.TrimSpace(attrVal(n, "rel")))

				isNext := rel == "next" ||
					containsAny(t, "trang sau", "trang tiep", "trang tiếp", "trang kế", "kế tiếp") ||
					containsAny(v, "trang sau", "trang tiep", "trang tiếp", "trang kế", "kế tiếp") ||
					(t == "next" || v == "next" || t == ">" || t == ">>") ||
					strings.Contains(class, "pagination-next") || strings.Contains(class, "next-page")

				if isNext {
					nextCandidates = append(nextCandidates, n)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(doc)

	if nextDisabled {
		return false, "", nil, extractTotalRows(docTextBuilder.String())
	}

	for _, n := range nextCandidates {
		name := strings.ToLower(strings.TrimSpace(attrVal(n, "name")))
		id := strings.ToLower(strings.TrimSpace(attrVal(n, "id")))
		val := strings.ToLower(strings.TrimSpace(attrVal(n, "value")))
		class := strings.ToLower(strings.TrimSpace(attrVal(n, "class")))
		href := strings.TrimSpace(attrVal(n, "href"))
		onclick := strings.TrimSpace(attrVal(n, "onclick"))
		disabled := hasAttr(n, "disabled") ||
			containsAny(class, "disabled", "inactive", "hidden", "next_disabled", "next-disabled") ||
			containsAny(name, "next_disabled", "next-disabled") ||
			containsAny(id, "next_disabled", "next-disabled") ||
			containsAny(val, "next_disabled", "next-disabled")

		if disabled {
			continue
		}
		if n.Data == "a" && (href == "" || href == "#") && onclick == "" {
			continue
		}

		hasNext = true
		if href != "" && !hasBlockedURLScheme(href) && href != "#" {
			nextAction = href
		}
		if form, err := ExtractHistoryForm(markup); err == nil && form.Action != "" {
			if nextAction == "" {
				nextAction = form.Action
			}
			nextFields = cloneFields(form.Fields)
			nextFields["_raw"] = "true"
			if ev := extractEventName(onclick); ev != "" {
				nextFields["dse_nextEventName"] = ev
			} else if ev := extractEventName(href); ev != "" {
				nextFields["dse_nextEventName"] = ev
			} else {
				nextFields["dse_nextEventName"] = "nextPage"
			}
		}
		break
	}

	fullText := docTextBuilder.String()
	totalRows = extractTotalRows(fullText)

	return
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walkText func(*html.Node)
	walkText = func(curr *html.Node) {
		if curr.Type == html.TextNode {
			b.WriteString(curr.Data)
			b.WriteString(" ")
		}
		for c := curr.FirstChild; c != nil; c = c.NextSibling {
			walkText(c)
		}
	}
	walkText(n)
	return strings.TrimSpace(b.String())
}

func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

func hasBlockedURLScheme(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "data:") || strings.HasPrefix(lower, "vbscript:")
}

func extractEventName(s string) string {
	patterns := []string{
		"dse_nextEventName='",
		`dse_nextEventName="`,
		"submitEvent('",
		`submitEvent("`,
		"doPage('",
		`doPage("`,
		"doSubmit('",
		`doSubmit("`,
	}
	for _, p := range patterns {
		idx := strings.Index(s, p)
		if idx >= 0 {
			rem := s[idx+len(p):]
			end := strings.IndexAny(rem, `'"`)
			if end > 0 {
				return rem[:end]
			}
		}
	}
	return ""
}

func extractTotalRows(text string) int {
	normalized := strings.ToLower(text)
	indicators := []string{"tổng số dòng:", "tổng số bản ghi:", "tổng số giao dịch:", "total rows:", "total records:"}
	for _, ind := range indicators {
		idx := strings.Index(normalized, ind)
		if idx >= 0 {
			rem := strings.TrimSpace(text[idx+len(ind):])
			var numStr string
			for _, r := range rem {
				if unicode.IsDigit(r) {
					numStr += string(r)
				} else if len(numStr) > 0 {
					break
				}
			}
			if num, err := strconv.Atoi(numStr); err == nil && num > 0 {
				return num
			}
		}
	}
	return 0
}
