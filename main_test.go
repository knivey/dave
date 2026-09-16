package main

import (
	"testing"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
)

func TestAccountFromEvent(t *testing.T) {
	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})

	t.Run("extended-join param", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "shrew", "Ron"}}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("extended-join star means unauthenticated", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "*", "Ron"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("account-notify param", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_ACCOUNT, Source: &girc.Source{Name: "shrew"}, Params: []string{"shrew"}}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("account-notify logout", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_ACCOUNT, Source: &girc.Source{Name: "shrew"}, Params: []string{"*"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("account message tag", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "lol"}}
		e.Tags = girc.Tags{"account": "shrew"}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("no account info falls back to empty girc state", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "lol"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("nil source does not panic", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Params: []string{"#gay", "lol"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})
}
