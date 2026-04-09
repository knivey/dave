package tables

type Alignment int

const (
	AlignLeft Alignment = iota
	AlignRight
	AlignCenter
)

type TableCell struct {
	Text  string
	Align Alignment
}

type TableRow []TableCell

type TableData struct {
	Rows           []TableRow
	HeaderRowCount int
}
