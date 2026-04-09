package tables

import (
	"strings"
	"unicode/utf8"

	"github.com/knivey/dave/MarkdownToIRC/irc"
)

const (
	// Box drawing characters for tables
	boxTL             = "┌" // Top-left corner
	boxTR             = "┐" // Top-right corner
	boxBL             = "└" // Bottom-left corner
	boxBR             = "┘" // Bottom-right corner
	boxV              = "│" // Vertical line
	boxH              = "─" // Horizontal line
	boxT              = "┬" // T-junction (top)
	boxC              = "├" // T-junction (left)
	boxR              = "┤" // T-junction (right)
	boxX              = "┼" // Cross (center)
	boxB              = "┴" // T-junction (bottom)
	maxTableCellWidth = 40
)

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func plainLength(s string) int {
	return utf8.RuneCountInString(irc.StripCodes(s))
}

func RenderTable(data TableData) string {
	rows := data.Rows
	headerRowCount := data.HeaderRowCount
	headerRowIdx := -1
	if headerRowCount > 0 {
		headerRowIdx = headerRowCount - 1
	}

	if len(rows) == 0 {
		return ""
	}

	numCols := 0
	for _, row := range rows {
		if len(row) > numCols {
			numCols = len(row)
		}
	}
	if numCols == 0 {
		return ""
	}

	colWidths := make([]int, numCols)
	for _, row := range rows {
		for i, cell := range row {
			lines := strings.Split(cell.Text, "\n")
			for _, line := range lines {
				w := plainLength(line)
				if w > colWidths[i] {
					colWidths[i] = min(w, maxTableCellWidth)
				}
			}
		}
	}

	// Build border components for each column
	var colSegments []string
	for _, cw := range colWidths {
		colSegments = append(colSegments, strings.Repeat(boxH, cw+2))
	}
	// Top: ┌───┬───┐
	topBorder := boxTL + strings.Join(colSegments, boxT) + boxTR
	// Middle: ├────┼────┤
	middleBorder := boxC + strings.Join(colSegments, boxX) + boxR
	// Bottom: └────┴────┘
	bottomBorder := boxBL + strings.Join(colSegments, boxB) + boxBR

	var lines []string
	lines = append(lines, topBorder)

	for ri, row := range rows {
		if ri > 0 && ri == headerRowIdx+1 {
			lines = append(lines, middleBorder)
		}

		var cellLines [][]string
		maxCellLines := 1

		for ci := 0; ci < numCols; ci++ {
			var text string
			if ci < len(row) {
				text = row[ci].Text
			}
			cw := colWidths[ci]
			wrapped := wrapCellText(text, cw)
			cellLines = append(cellLines, wrapped)
			if len(wrapped) > maxCellLines {
				maxCellLines = len(wrapped)
			}
		}

		for li := 0; li < maxCellLines; li++ {
			var rowLine strings.Builder
			rowLine.WriteString(boxV)
			for ci := 0; ci < numCols; ci++ {
				var line string
				var align Alignment
				if ci < len(row) {
					align = row[ci].Align
				}
				if li < len(cellLines[ci]) {
					line = cellLines[ci][li]
				}
				cw := colWidths[ci]

				var padded string
				switch align {
				case AlignRight:
					padded = FormatTableLine(line, cw, "right")
				case AlignCenter:
					padded = FormatTableLine(line, cw, "center")
				default:
					padded = FormatTableLine(line, cw, "left")
				}

				rowLine.WriteString(" " + padded + " " + boxV)
			}
			lines = append(lines, rowLine.String())
		}
	}

	lines = append(lines, bottomBorder)

	return "\n" + strings.Join(lines, "\n")
}

func wrapCellText(text string, maxWidth int) []string {
	if text == "" {
		return []string{""}
	}

	var allLines []string
	for _, segment := range strings.Split(text, "\n") {
		if segment == "" {
			allLines = append(allLines, "")
			continue
		}
		wrapped := irc.WordWrap(segment, maxWidth)
		allLines = append(allLines, wrapped...)
	}

	if len(allLines) == 0 {
		allLines = []string{""}
	}
	return allLines
}

func FormatTableLine(text string, width int, align string) string {
	plainW := utf8.RuneCountInString(irc.StripCodes(text))
	if plainW >= width {
		return text
	}

	pad := width - plainW
	var result strings.Builder

	switch align {
	case "right":
		result.WriteString(strings.Repeat(" ", pad))
		result.WriteString(text)
	case "center":
		left := pad / 2
		right := pad - left
		result.WriteString(strings.Repeat(" ", left))
		result.WriteString(text)
		result.WriteString(strings.Repeat(" ", right))
	default:
		result.WriteString(text)
		result.WriteString(strings.Repeat(" ", pad))
	}

	return result.String()
}
