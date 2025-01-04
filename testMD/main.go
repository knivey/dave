package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/chzchzchz/sitbot/ascii"
	markdowntoirc "github.come/knivey/dave/MarkdownToIRC"
)

func main() {
	test, err := os.ReadFile("test.md")
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Print(string(test))
	fmt.Print("\n========================Formatted:===================\n")

	out := markdowntoirc.MarkdownToIRC(string(test))
	out = strings.ReplaceAll(out, "\x03", "\x1b[033m[C]\x1b[0m")
	out = strings.ReplaceAll(out, "\x02", "\x1b[034m[B]\x1b[0m")
	out = strings.ReplaceAll(out, "\x1F", "\x1b[035m[U]\x1b[0m")
	out = strings.ReplaceAll(out, "\x1D", "\x1b[036m[I]\x1b[0m")
	fmt.Print(out)

	fmt.Print("\n========================Formatted:===================\n")

	out = markdowntoirc.MarkdownToIRC(string(test))
	//sitbot/ascii can't handle extended colors
	out = strings.ReplaceAll(out, "\x030,90", "\x030,02")
	ascii, _ := ascii.NewASCII(out)
	fmt.Print(string(ascii.AnsiBytes()))

}
