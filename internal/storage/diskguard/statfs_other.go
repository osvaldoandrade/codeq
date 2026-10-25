//go:build !unix

package diskguard

import "errors"

// Statfs is unsupported off Unix; the guard then fails open.
func Statfs(string) (Usage, error) {
	return Usage{}, errors.New("statfs: unsupported platform")
}
