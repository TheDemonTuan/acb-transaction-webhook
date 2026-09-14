package acb

// PaginationCursor tracks ACB history navigation state across multi-page queries.
type PaginationCursor struct {
	Action         string
	Fields         map[string]string
	PageNumber     int
	TotalRowsSeen  int
	CumulativeRows int
	HasNext        bool
	Complete       bool
	Truncated      bool
}

// NewPaginationCursor creates an initial pagination cursor from the base form action and fields.
func NewPaginationCursor(action string, fields map[string]string) *PaginationCursor {
	return &PaginationCursor{
		Action:  action,
		Fields:  cloneFields(fields),
		HasNext: true,
	}
}

// Step updates the cursor with parsed results from the current page.
func (c *PaginationCursor) Step(page HistoryPageResult, newRows int) {
	c.PageNumber++
	c.CumulativeRows += newRows
	if page.TotalRows > c.TotalRowsSeen {
		c.TotalRowsSeen = page.TotalRows
	}

	if !page.HasNext {
		c.HasNext = false
		c.Action = ""
		c.Fields = nil
		if c.TotalRowsSeen > 0 && c.CumulativeRows < c.TotalRowsSeen {
			c.Truncated = true
			c.Complete = false
		} else {
			c.Complete = true
			c.Truncated = false
		}
		return
	}

	// Defensive stop if HasNext is reported but navigation target is missing.
	if page.NextAction == "" && len(page.NextFields) == 0 {
		c.HasNext = false
		c.Truncated = true
		c.Complete = false
		c.Action = ""
		c.Fields = nil
		return
	}

	c.HasNext = true
	if page.NextAction != "" {
		c.Action = page.NextAction
	}
	c.Fields = cloneFields(page.NextFields)
}
