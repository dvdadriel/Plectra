package player

import "unsafe"

// f32slice views miniaudio's output byte buffer as float32 without copying.
// Safe because the device is configured as FormatF32 and the slice never
// outlives the callback.
func f32slice(b []byte, n int) []float32 {
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), n)
}
