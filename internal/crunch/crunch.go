// Package crunch decodes Unity's crunch-compressed texture formats (DXT1Crunched,
// DXT5Crunched) to raw DXTn block bytes via a cgo binding to Binomial LLC's crn_decomp.h
// (zlib license), vendored under unitycrunch/ -- specifically Unity's own fork of the format,
// which is what Dofus 3's Props bundles use. No pure-Go crunch decoder exists; every other
// Unity-asset tool (AssetStudio, UnityPy) wraps this same native library rather than
// reimplementing its Huffman/codebook decoding by hand.
//
// Output is raw DXTn block data for one mip level, not pixels -- feed it to the matching
// block decoder (e.g. dds.DecompressDXT5) same as any other DXT5 texture.
package crunch

/*
#cgo CXXFLAGS: -std=c++11 -fno-strict-aliasing
#include <stdint.h>
#include <stdlib.h>
#include "shim.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Decode decompresses one mip level (levelIndex, 0 = full resolution) of Unity-crunch-compressed
// data to raw DXTn block bytes.
func Decode(data []byte, levelIndex uint32) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("crunch: empty input")
	}

	var outPtr *C.uint8_t
	var outSize C.uint32_t
	ok := C.doduda_unity_crunch_decode(
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.uint32_t(len(data)),
		C.uint32_t(levelIndex),
		&outPtr,
		&outSize,
	)
	if ok == 0 {
		return nil, fmt.Errorf("crunch: decode failed (level %d, %d input bytes)", levelIndex, len(data))
	}
	defer C.doduda_crunch_free(outPtr)

	return C.GoBytes(unsafe.Pointer(outPtr), C.int(outSize)), nil
}
