package main

import (
	"github.com/lrstanley/girc"
)

func support(network Network, client *girc.Client, event girc.Event, _ ...string) {
	var msg string
	readConfig(func() { msg = config.Notices.Support })
	client.Cmd.Reply(event, msg)
}
