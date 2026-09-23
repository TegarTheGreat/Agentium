//go:build !linux && !darwin

package main

import (
	"errors"
	"io"
	"os"
)

var errEOF = io.EOF

func makeRaw(*os.File) (func(), error) { return nil, errors.New("line editing not supported") }

func termWidth(*os.File) int { return 80 }

const lineEditing = false
