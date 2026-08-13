#ifndef DODUDA_CRUNCH_SHIM_H
#define DODUDA_CRUNCH_SHIM_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// Decodes one mip level of a Unity-crunch-compressed texture (m_TextureFormat
// DXT1Crunched/DXT5Crunched) to raw DXTn block bytes. On success returns 1 and
// sets *out/*out_size to a buffer owned by the caller -- release it with
// doduda_crunch_free. On failure returns 0 and leaves *out untouched.
int doduda_unity_crunch_decode(const uint8_t *data, uint32_t data_size,
                                uint32_t level_index, uint8_t **out,
                                uint32_t *out_size);

void doduda_crunch_free(uint8_t *p);

#ifdef __cplusplus
}
#endif

#endif
