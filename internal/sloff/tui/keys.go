package tui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up      key.Binding
	Down    key.Binding
	Top     key.Binding
	Bottom  key.Binding
	LogFile key.Binding
	Quit    key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up:      key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("↑/k", "up")),
		Down:    key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("↓/j", "down")),
		Top:     key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "top")),
		Bottom:  key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "bottom")),
		LogFile: key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "view log")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q/^C", "quit")),
	}
}
