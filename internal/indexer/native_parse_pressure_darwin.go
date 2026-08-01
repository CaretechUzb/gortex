//go:build darwin && cgo

package indexer

/*
#include <malloc/malloc.h>
*/
import "C"

const nativeAllocatorPressureReliefSupported = true

func nativeAllocatorPressureRelief() uintptr {
	return uintptr(C.malloc_zone_pressure_relief((*C.malloc_zone_t)(nil), 0))
}
