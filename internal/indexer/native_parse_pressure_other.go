//go:build !darwin || !cgo

package indexer

const nativeAllocatorPressureReliefSupported = false

func nativeAllocatorPressureRelief() uintptr { return 0 }
