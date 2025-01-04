package markdowntoirc

import (
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
)

var c = chroma.MustParseColour

type IrcColorTable map[chroma.Colour]string

var IrcTable = IrcColorTable{
	c("#FFFFFF"): "00",
	c("#000000"): "01",
	c("#00007F"): "02",
	c("#009300"): "03",
	c("#FF0000"): "04",
	c("#7F0000"): "05",
	c("#9C009C"): "06",
	c("#FC7F00"): "07",
	c("#FFFF00"): "08",
	c("#00FC00"): "09",
	c("#009393"): "10",
	c("#00FFFF"): "11",
	c("#0000FC"): "12",
	c("#FF00FF"): "13",
	c("#7F7F7F"): "14",
	c("#D2D2D2"): "15",
}

func (table IrcColorTable) findClosest(seeking chroma.Colour) chroma.Colour {
	closestColour := chroma.Colour(0)
	closest := float64(math.MaxFloat64)
	for colour := range table {
		distance := colour.Distance(seeking)
		if distance < closest {
			closest = distance
			closestColour = colour
		}
	}
	return closestColour
}

type IRCStyle struct {
	Colour    string
	Bold      bool
	Italic    bool
	Underline bool
}

func IRCStyleFromEntry(entry chroma.StyleEntry) IRCStyle {
	return IRCStyle{
		Colour:    IrcTable[IrcTable.findClosest(entry.Colour)],
		Bold:      entry.Bold == chroma.Yes,
		Italic:    entry.Italic == chroma.Yes,
		Underline: entry.Underline == chroma.Yes,
	}
}

func (i IRCStyle) Format() (out string) {
	if i.Bold {
		out += "\x02"
	}
	if i.Italic {
		out += "\x1D"
	}
	if i.Underline {
		out += "\x1F"
	}
	if i.Colour != "" {
		out += "\x03" + i.Colour
	}
	return
}

func (i IRCStyle) DeltaFormat(old IRCStyle) (out string) {
	if i.Bold != old.Bold {
		out += "\x02"
	}
	if i.Italic != old.Italic {
		out += "\x1D"
	}
	if i.Underline != old.Underline {
		out += "\x1F"
	}
	if old.Colour != i.Colour {
		if i.Colour == "" {
			out += "\x03"
		} else {
			out += "\x03" + i.Colour
		}
	}
	return
}

type IRCFormatter struct {
}

func (*IRCFormatter) Format(w io.Writer, style *chroma.Style, it chroma.Iterator) (err error) {
	var lastSytle IRCStyle
	for token := it(); token != chroma.EOF; token = it() {
		entry := style.Get(token.Type)
		newStyle := IRCStyleFromEntry(entry)

		if lastSytle != newStyle {
			fmt.Fprint(w, newStyle.DeltaFormat(lastSytle))
		}
		//fmt.Fprint(w, "\x1b[032m("+token.Type.String()+")\x1b[0m")
		writeToken(w, newStyle, token.Value)
		lastSytle = newStyle
	}
	return nil
}

// not 100% perfect but good
func writeToken(w io.Writer, newStyle IRCStyle, text string) {
	text = strings.ReplaceAll(text, "\n", "\n"+newStyle.Format())
	fmt.Fprint(w, text)
}

var IRC16 = formatters.Register("irc16", &IRCFormatter{})
