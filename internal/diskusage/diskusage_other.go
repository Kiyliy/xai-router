//go:build !unix && !windows

package diskusage

import "errors"

func of(string) (Usage, error) { return Usage{}, errors.New("disk usage not supported on this platform") }
