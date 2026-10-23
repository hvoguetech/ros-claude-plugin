//go:build !unix

package fsutil

import "context"

// Lock is a no-op where advisory file locks aren't wired up (Windows is not a supported platform yet).
func Lock(ctx context.Context, path string) (func(), error) { return func() {}, nil }
