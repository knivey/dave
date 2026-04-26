package main

import (
	"github.com/lrstanley/girc"
)

func support(network Network, client *girc.Client, event girc.Event, _ ...string) {
	client.Cmd.Reply(event, "If you enjoy using dave, consider supporting development at https://patreon.com/shrew269 ❤️")
}