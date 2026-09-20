//go:build windows

package platform

import "unsafe"

// SetInformationJobObject takes a raw pointer and a size. Keeping the two
// unsafe expressions in one tiny file keeps the rest of the package readable
// and makes every unsafe use in this package greppable in one place.

func unsafePointerOf[T any](v *T) unsafe.Pointer { return unsafe.Pointer(v) }

func unsafeSizeOf[T any](v T) uintptr { return unsafe.Sizeof(v) }
