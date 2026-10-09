package tui

import "github.com/rossoctl/cortex/cmd/agentop/tui/table"

// noLimit is the width a cell function is handed for its full value: wider than anything
// agentop draws, and named so a call site says "no limit" rather than a width someone chose.
// Only for functions that cut to a width; never for one that pads to it.
const noLimit = 1 << 16

// rowBuilder builds one table row's cells and, while a search is set, their full values in
// step with them, so no cell can be added to a row without the search knowing what it holds.
// full is lazy: without a search no full value is computed.
type rowBuilder struct {
	row  table.Row
	full *table.SearchRow
}

func newRowBuilder(searching bool) rowBuilder {
	if searching {
		return rowBuilder{full: &table.SearchRow{}}
	}
	return rowBuilder{}
}

// cell adds a cell drawn whole.
func (b *rowBuilder) cell(shown string) {
	b.row = append(b.row, shown)
	if b.full != nil {
		b.full.Cells = append(b.full.Cells, "")
	}
}

// cut adds a cell the pane cut to fit, drawn as shown, whose full value full returns.
func (b *rowBuilder) cut(shown string, full func() string) {
	b.row = append(b.row, shown)
	if b.full != nil {
		b.full.Cells = append(b.full.Cells, full())
	}
}

// dropped records the full value of a column the pane had no room to draw.
func (b *rowBuilder) dropped(full func() string) {
	if b.full != nil {
		b.full.Dropped = append(b.full.Dropped, full())
	}
}

func (b *rowBuilder) searchRow() table.SearchRow {
	if b.full == nil {
		return table.SearchRow{}
	}
	return *b.full
}
