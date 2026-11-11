//go:build !windows

package main

import "errors"

func runScan(device, include string, list bool) error {
	return errors.New("--scan is Windows-only (NTFS MFT/USN)")
}
