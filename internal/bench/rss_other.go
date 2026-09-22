//go:build !linux && !darwin

package bench

import "os"

func maxRSS(*os.ProcessState) int64 { return 0 }
