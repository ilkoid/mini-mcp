//go:build !windows

package main

import (
	"errors"
	"time"
)

func runAgent(dbPath, device, include, agentName, uncBase string, watch time.Duration) error {
	return errors.New("--volume agent mode is Windows-only (NTFS MFT/USN)")
}
