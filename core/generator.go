package core

import "github.com/fsnotify/fsnotify"

type Generator interface {
	Init() error
	OnChange(appPath string, outputPath string, event fsnotify.Event) error
	Generate(appPath string, outputPath string) error
}
