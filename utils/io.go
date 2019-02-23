package utils

import "io"

// ConsoleWriter interface, that holde both output and error writers
type ConsoleWriter interface {
	Out() io.Writer
	Err() io.Writer
}
