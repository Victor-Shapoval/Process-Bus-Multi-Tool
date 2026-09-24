//go:build !linux && !darwin

package ptp

import (
	"fmt"
	"runtime"

	"pbmt/internal/application/ptpport"
)

func New(_, _, _ string) (ptpport.Transport, error) {
	return nil, fmt.Errorf("ptp transport is unsupported on %s", runtime.GOOS)
}

func NewGrandmaster(_, _ string) (ptpport.Transport, error) {
	return nil, fmt.Errorf("%w for PTP Grandmaster on %s; software fallback is disabled", ErrHardwareTimestampingUnavailable, runtime.GOOS)
}
