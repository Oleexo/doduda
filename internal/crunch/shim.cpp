// Thin extern "C" wrapper around Binomial LLC's crn_decomp.h (zlib license, vendored under
// unitycrunch/), specifically its Unity fork -- the variant Dofus 3's Props bundles use
// (m_TextureFormat DXT5Crunched). crn_decomp.h is a single-header, dependency-free C++ library;
// this file only adapts its API to a flat C ABI cgo can call.
#include "shim.h"

#include <algorithm>
#include <cstdint>
#include <cstdlib>

#include "unitycrunch/crn_decomp.h"

extern "C" int doduda_unity_crunch_decode(const uint8_t *data, uint32_t data_size,
                                           uint32_t level_index, uint8_t **out,
                                           uint32_t *out_size) {
  unitycrnd::crn_texture_info tex_info;
  if (!unitycrnd::crnd_get_texture_info(data, data_size, &tex_info)) {
    return 0;
  }

  unitycrnd::crnd_unpack_context ctx = unitycrnd::crnd_unpack_begin(data, data_size);
  if (!ctx) {
    return 0;
  }

  const uint32_t width = std::max(1u, tex_info.m_width >> level_index);
  const uint32_t height = std::max(1u, tex_info.m_height >> level_index);
  const uint32_t blocks_x = std::max(1u, (width + 3) >> 2);
  const uint32_t blocks_y = std::max(1u, (height + 3) >> 2);
  const uint32_t row_pitch =
      blocks_x * unitycrnd::crnd_get_bytes_per_dxt_block(tex_info.m_format);
  const uint32_t total_size = row_pitch * blocks_y;

  uint8_t *buf = static_cast<uint8_t *>(malloc(total_size));
  if (!buf) {
    unitycrnd::crnd_unpack_end(ctx);
    return 0;
  }

  void *dst = buf;
  bool ok = unitycrnd::crnd_unpack_level(ctx, &dst, total_size, row_pitch, level_index);
  unitycrnd::crnd_unpack_end(ctx);

  if (!ok) {
    free(buf);
    return 0;
  }

  *out = buf;
  *out_size = total_size;
  return 1;
}

extern "C" void doduda_crunch_free(uint8_t *p) { free(p); }
