package main

import (
	"fmt"
	"strings"

	"github.com/knivey/dave/MarkdownToIRC/tables"
)

func tableFunc(slice any, columns string) (string, error) {
	if slice == nil {
		return "", nil
	}
	items, ok := slice.([]any)
	if !ok {
		return "", fmt.Errorf("table: expected array, got %T", slice)
	}
	if len(items) == 0 {
		return "", nil
	}

	colNames := strings.Split(columns, ",")
	for i := range colNames {
		colNames[i] = strings.TrimSpace(colNames[i])
	}

	headerRow := make(tables.TableRow, len(colNames))
	for i, name := range colNames {
		headerRow[i] = tables.TableCell{Text: name}
	}

	var rows []tables.TableRow
	rows = append(rows, headerRow)

	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("table: expected object, got %T", item)
		}
		row := make(tables.TableRow, len(colNames))
		for i, name := range colNames {
			val := obj[name]
			text := ""
			if val != nil {
				text = fmt.Sprintf("%v", val)
			}
			row[i] = tables.TableCell{Text: text}
		}
		rows = append(rows, row)
	}

	return tables.RenderTable(tables.TableData{
		Rows:           rows,
		HeaderRowCount: 1,
	}), nil
}

var toolTemplateFuncMap = map[string]any{
	"table": tableFunc,
}
