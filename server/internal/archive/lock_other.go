//go:build !(linux || darwin || freebsd)

package archive

// lockFolder can't lock here; the one-writer rule is left to the operator.
func lockFolder(root string) (func() error, error) {
	return func() error { return nil }, nil
}
