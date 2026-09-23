//go:build !unix

package tool

import "errors"

func syscallMkfifo(string) error { return errors.New("unsupported") }
