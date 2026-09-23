//go:build !linux && !darwin && !windows

package main

import (
	"errors"
	"io"
	"os"
	"time"
)

var errEOF = io.EOF

func makeRaw(*os.File) (func(), error) { return nil, errors.New("line editing not supported") }

func termWidth(*os.File) int { return 80 }

const lineEditing = false

func inputReady(*os.File, time.Duration) bool { return true }
