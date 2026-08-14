package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	"charm.land/log/v2"
	"github.com/kvarenzn/ssm/uni"
	"github.com/xypwn/filediver/dds"

	"github.com/dofusdude/doduda/internal/crunch"
)

func unpackUnityImagesNative(inputDir string, outputDir string) error {
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".imagebundle") {
			continue
		}

		inputPath := filepath.Join(inputDir, entry.Name())
		if err := unpackUnityImageBundleNative(inputPath, outputDir); err != nil {
			return fmt.Errorf("unpack %s: %w", entry.Name(), err)
		}
	}

	return nil
}

func unpackUnityImageBundleNative(inputPath string, outputDir string) error {
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}

	assetsManager := uni.NewAssetsManager()
	// uni's own LoadDataFromHandler only understands LZ4/uncompressed UnityFS blocks and errors
	// with "LZMA unsupported" on bundles that use LZMA compression (observed on
	// Content/Animations/Props/*.bundle). loadUnityAssetFilesNative (unity_bundle_native.go) is
	// doduda's own UnityFS container parser, written for the data-bundle path specifically because
	// it needs LZMA support uni.LoadDataFromHandler doesn't have -- reuse it here instead of
	// duplicating a second container parser.
	if err := loadUnityAssetFilesNative(data, inputPath, assetsManager); err != nil {
		return err
	}

	for _, assetFile := range assetsManager.AssetFiles {
		for _, objectInfo := range assetFile.ObjectInfos {
			reader := uni.NewObjectReader(assetFile.Reader.BinaryReader, assetFile, objectInfo)
			switch objectInfo.ClassID {
			case uni.ClassIDAssetBundle:
				assetFile.AddObject(uni.NewAssetBundle(reader))
			case uni.ClassIDTexture2D:
				assetFile.AddObject(uni.NewTexture2D(reader))
			case uni.ClassIDSprite:
				assetFile.AddObject(uni.NewSprite(reader))
			default:
				assetFile.AddObject(uni.NewObject(reader))
			}
		}
	}

	targetDir := outputDir
	if subdir := unityImageResolutionSubdir(filepath.Base(inputPath)); subdir != "" {
		targetDir = filepath.Join(outputDir, subdir)
	}
	if err := os.MkdirAll(targetDir, os.ModePerm); err != nil {
		return err
	}
	layerOrder := unityBundleLayerDirs(filepath.Base(inputPath))

	textureCache := make(map[*uni.Texture2D]image.Image)
	nameStates := make(map[string]*unityNameState)
	// Textures already consumed by at least one sprite crop. Without this, the whole-Texture2D
	// pass below would additionally dump the raw, uncropped atlas for every texture a sprite
	// already covered -- redundant at best (a full 4096x4096 atlas next to its own cropped
	// pieces), and before cropping was added this only looked harmless because the atlas dump
	// happened to be byte-identical to the sprite dump.
	usedTextures := make(map[*uni.Texture2D]bool)
	var spriteMetas []unitySpriteMeta

	for _, assetFile := range assetsManager.AssetFiles {
		for _, object := range assetFile.Objects {
			sprite, ok := object.(*uni.Sprite)
			if !ok {
				continue
			}

			spriteImage, atlasTexture, atlasBounds, err := unityDecodeSpriteImage(sprite, textureCache)
			if err != nil {
				// One malformed/unsupported sprite (an unusual texture format, a metadata
				// mismatch on an outlier asset, ...) used to abort the whole bundle here,
				// throwing away every other sprite that decoded fine -- observed on a 16 MB
				// bundle with thousands of good sprites and exactly one bad one. Skip and keep
				// going instead; the caller sees the miss via this warning and the output file
				// count coming up short of the sprite count.
				log.Warnf("skip sprite %q: %s", sprite.Name, err)
				continue
			}
			usedTextures[atlasTexture] = true

			outputName := unityOutputImageName(sprite.Name, unityObjectFallbackName(sprite.GetObject()))
			outputPath := unityNextImagePath(targetDir, outputName, spriteImage, nameStates, layerOrder)
			if err := unityWritePNG(outputPath, spriteImage); err != nil {
				return err
			}

			spriteMetas = append(spriteMetas, unitySpriteMetaFrom(sprite, atlasTexture, atlasBounds, outputPath, targetDir))
		}
	}

	if len(spriteMetas) > 0 {
		if err := unityWriteSpriteSidecar(filepath.Base(inputPath), targetDir, spriteMetas); err != nil {
			return err
		}
	}

	if len(layerOrder) > 0 {
		return nil
	}

	for _, assetFile := range assetsManager.AssetFiles {
		for _, object := range assetFile.Objects {
			texture, ok := object.(*uni.Texture2D)
			if !ok || usedTextures[texture] {
				continue
			}

			textureImage, err := unityDecodeTextureImage(texture, 0, 0)
			if err != nil {
				log.Warnf("skip texture %q: %s", texture.Name, err)
				continue
			}

			outputName := unityOutputImageName(texture.Name, unityObjectFallbackName(texture.GetObject()))
			outputPath := unityNextImagePath(targetDir, outputName, textureImage, nameStates, layerOrder)
			if err := unityWritePNG(outputPath, textureImage); err != nil {
				return err
			}
		}
	}

	return nil
}

// unityDecodeSpriteImage decodes (and caches) the sprite's backing atlas texture, then crops out
// just the sprite's own sub-rectangle -- sprite.RenderData.TextureRect -- instead of returning the
// whole shared atlas. Before this, every sprite packed into the same atlas produced an identical
// full-texture PNG (a 4096x4096 sheet with dozens of unrelated objects on it, ~15-30% opaque,
// observed on every Content/Animations/Props bundle); TextureRect was read only as a width/height
// hint to disambiguate the texture's own field layout, never as a crop box. Returns the atlas
// texture and its pixel bounds alongside the crop so the caller can build sidecar metadata and
// dedupe the separate whole-Texture2D export pass against textures already covered by a sprite.
func unityDecodeSpriteImage(sprite *uni.Sprite, textureCache map[*uni.Texture2D]image.Image) (image.Image, *uni.Texture2D, image.Rectangle, error) {
	if sprite == nil || sprite.RenderData == nil || sprite.RenderData.Texture == nil {
		return nil, nil, image.Rectangle{}, fmt.Errorf("sprite has no render data texture")
	}

	textureObject, ok := sprite.RenderData.Texture.Get().(*uni.Texture2D)
	if !ok || textureObject == nil {
		return nil, nil, image.Rectangle{}, fmt.Errorf("sprite texture reference is not a Texture2D")
	}

	atlasImage, ok := textureCache[textureObject]
	if !ok {
		var err error
		hintWidth, hintHeight := 0, 0
		if sprite.RenderData.TextureRect != nil {
			hintWidth = int(math.Round(float64(sprite.RenderData.TextureRect.Width)))
			hintHeight = int(math.Round(float64(sprite.RenderData.TextureRect.Height)))
		}
		atlasImage, err = unityDecodeTextureImage(textureObject, hintWidth, hintHeight)
		if err != nil {
			return nil, nil, image.Rectangle{}, err
		}
		textureCache[textureObject] = atlasImage
	}

	atlasBounds := atlasImage.Bounds()

	rect := sprite.RenderData.TextureRect
	if rect == nil || rect.Width <= 0 || rect.Height <= 0 {
		return nil, nil, image.Rectangle{}, fmt.Errorf("sprite has no usable texture rect")
	}

	atlasNRGBA, ok := atlasImage.(*image.NRGBA)
	if !ok {
		return nil, nil, image.Rectangle{}, fmt.Errorf("atlas texture decoded to unsupported image type %T", atlasImage)
	}

	// Unity texture space is bottom-left origin (V increases upward); the decoded atlas image
	// (already flipped top-down by unityFlipVerticalNRGBA in unityDecodeTextureImage) is top-left
	// origin like every Go image, so the rect's Y needs flipping to locate it in the decoded image.
	left := int(math.Round(float64(rect.X)))
	width := int(math.Round(float64(rect.Width)))
	height := int(math.Round(float64(rect.Height)))
	top := atlasBounds.Dy() - int(math.Round(float64(rect.Y))) - height

	cropRect := image.Rect(left, top, left+width, top+height)
	clamped := cropRect.Intersect(atlasBounds)
	if clamped.Empty() {
		return nil, nil, image.Rectangle{}, fmt.Errorf("sprite texture rect %v does not intersect atlas bounds %v", cropRect, atlasBounds)
	}
	if clamped != cropRect {
		log.Warnf("sprite %q texture rect %v clamped to atlas bounds %v", sprite.Name, cropRect, atlasBounds)
	}

	cropped := imageCropNRGBA(atlasNRGBA, clamped)

	if sprite.RenderData.SettingsRaw != nil {
		switch sprite.RenderData.SettingsRaw.PackingRotation {
		case uni.SPRFlipHorizontal:
			cropped = imageFlipHorizontalNRGBA(cropped)
		case uni.SPRFlipVertical:
			cropped = unityFlipVerticalNRGBA(cropped)
		case uni.SPRRotate180:
			cropped = imageFlipHorizontalNRGBA(unityFlipVerticalNRGBA(cropped))
		case uni.SPRRotate90:
			cropped = imageRotate90ClockwiseNRGBA(cropped)
		}
	}

	return cropped, textureObject, atlasBounds, nil
}

// imageCropNRGBA copies out a sub-rectangle into a new, independently-backed image. rect must
// already be clamped to src's bounds.
func imageCropNRGBA(src *image.NRGBA, rect image.Rectangle) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	rowSize := rect.Dx() * 4
	for y := 0; y < rect.Dy(); y++ {
		srcStart := (rect.Min.Y+y)*src.Stride + rect.Min.X*4
		dstStart := y * out.Stride
		copy(out.Pix[dstStart:dstStart+rowSize], src.Pix[srcStart:srcStart+rowSize])
	}
	return out
}

// imageFlipHorizontalNRGBA mirrors an image left-right, undoing SpritePackingRotation.FlipHorizontal.
func imageFlipHorizontalNRGBA(src *image.NRGBA) *image.NRGBA {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		srcRowStart := (bounds.Min.Y + y) * src.Stride
		dstRowStart := y * out.Stride
		for x := 0; x < width; x++ {
			srcIdx := srcRowStart + (bounds.Min.X+x)*4
			dstIdx := dstRowStart + (width-1-x)*4
			copy(out.Pix[dstIdx:dstIdx+4], src.Pix[srcIdx:srcIdx+4])
		}
	}
	return out
}

// imageRotate90ClockwiseNRGBA rotates an image 90 degrees clockwise, undoing
// SpritePackingRotation.Rotate90 (the atlas packer rotates a sprite 90 degrees counter-clockwise
// to fit it more tightly; this is the inverse). Output width/height are swapped from the input.
func imageRotate90ClockwiseNRGBA(src *image.NRGBA) *image.NRGBA {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, height, width))
	for y := 0; y < height; y++ {
		srcRowStart := (bounds.Min.Y + y) * src.Stride
		for x := 0; x < width; x++ {
			srcIdx := srcRowStart + (bounds.Min.X+x)*4
			dstX := height - 1 - y
			dstY := x
			dstIdx := dstY*out.Stride + dstX*4
			copy(out.Pix[dstIdx:dstIdx+4], src.Pix[srcIdx:srcIdx+4])
		}
	}
	return out
}

// unitySpriteMeta is the sidecar record for one cropped sprite, written to <bundle>.sprites.json
// alongside the PNGs. It is the only surviving link between a filename and where it came from --
// without it, PathID, atlas membership, rect/pivot/packing data are all discarded once the PNG is
// written.
type unitySpriteMeta struct {
	Name            string  `json:"name"`
	PathID          int64   `json:"pathId"`
	TextureName     string  `json:"textureName"`
	AtlasWidth      int     `json:"atlasWidth"`
	AtlasHeight     int     `json:"atlasHeight"`
	RectX           float32 `json:"rectX"`
	RectY           float32 `json:"rectY"`
	RectWidth       float32 `json:"rectWidth"`
	RectHeight      float32 `json:"rectHeight"`
	OffsetX         float32 `json:"offsetX"`
	OffsetY         float32 `json:"offsetY"`
	PivotX          float32 `json:"pivotX"`
	PivotY          float32 `json:"pivotY"`
	PackingRotation string  `json:"packingRotation"`
	PixelsToUnits   float32 `json:"pixelsToUnits"`
	OutputFile      string  `json:"outputFile"`
}

func unitySpriteMetaFrom(sprite *uni.Sprite, atlasTexture *uni.Texture2D, atlasBounds image.Rectangle, outputPath string, targetDir string) unitySpriteMeta {
	meta := unitySpriteMeta{
		Name:          sprite.Name,
		PathID:        sprite.GetObject().PathID,
		AtlasWidth:    atlasBounds.Dx(),
		AtlasHeight:   atlasBounds.Dy(),
		PixelsToUnits: sprite.PixelsToUnits,
	}
	if atlasTexture != nil {
		meta.TextureName = atlasTexture.Name
	}
	if sprite.RenderData != nil && sprite.RenderData.TextureRect != nil {
		meta.RectX = sprite.RenderData.TextureRect.X
		meta.RectY = sprite.RenderData.TextureRect.Y
		meta.RectWidth = sprite.RenderData.TextureRect.Width
		meta.RectHeight = sprite.RenderData.TextureRect.Height
	}
	if sprite.Offset != nil {
		meta.OffsetX = sprite.Offset.X
		meta.OffsetY = sprite.Offset.Y
	}
	if sprite.Pivot != nil {
		meta.PivotX = sprite.Pivot.X
		meta.PivotY = sprite.Pivot.Y
	}
	if sprite.RenderData != nil && sprite.RenderData.SettingsRaw != nil {
		meta.PackingRotation = packingRotationString(sprite.RenderData.SettingsRaw.PackingRotation)
	}
	if rel, err := filepath.Rel(targetDir, outputPath); err == nil {
		meta.OutputFile = rel
	} else {
		meta.OutputFile = outputPath
	}
	return meta
}

func packingRotationString(rotation uni.SpritePackingRotation) string {
	switch rotation {
	case uni.SPRNone:
		return "none"
	case uni.SPRFlipHorizontal:
		return "flipHorizontal"
	case uni.SPRFlipVertical:
		return "flipVertical"
	case uni.SPRRotate180:
		return "rotate180"
	case uni.SPRRotate90:
		return "rotate90"
	default:
		return "unknown"
	}
}

func unityWriteSpriteSidecar(bundleName string, targetDir string, metas []unitySpriteMeta) error {
	base := strings.TrimSuffix(bundleName, filepath.Ext(bundleName))
	path := filepath.Join(targetDir, base+".sprites.json")

	data, err := json.MarshalIndent(metas, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, os.ModePerm)
}

func unityDecodeTextureImage(texture *uni.Texture2D, hintWidth int, hintHeight int) (image.Image, error) {
	raw, err := unityReadTextureData(texture)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("texture data is empty")
	}

	meta := unityNormalizedTextureMeta(texture, len(raw), hintWidth, hintHeight)
	if meta.width <= 0 || meta.height <= 0 {
		return nil, fmt.Errorf("invalid texture dimensions %dx%d", meta.width, meta.height)
	}

	switch meta.format {
	case uni.BC7:
		decoded := image.NewNRGBA(image.Rect(0, 0, meta.width, meta.height))
		size := meta.completeSize
		if size <= 0 || size > len(raw) {
			size = len(raw)
		}
		if _, err := dds.DecompressBC7(decoded.Pix, bytes.NewReader(raw[:size]), meta.width, meta.height, dds.Info{ColorModel: color.NRGBAModel}); err != nil {
			return nil, err
		}
		return unityFlipVerticalNRGBA(decoded), nil
	case uni.DXT5:
		// Same shape as the BC7 case above: uni.DecodeTexture2D has no DXT/BC1-5 support at all
		// (decode_texture2d.go only implements Alpha8/RGB24/RGBA32/ARGB32/ETC/ASTC), but the dds
		// package doduda already imports for BC7 also implements DXT5 -- just wasn't wired in yet.
		decoded := image.NewNRGBA(image.Rect(0, 0, meta.width, meta.height))
		size := meta.completeSize
		if size <= 0 || size > len(raw) {
			size = len(raw)
		}
		if _, err := dds.DecompressDXT5(decoded.Pix, bytes.NewReader(raw[:size]), meta.width, meta.height, dds.Info{ColorModel: color.NRGBAModel}); err != nil {
			return nil, err
		}
		return unityFlipVerticalNRGBA(decoded), nil
	case uni.DXT1:
		decoded := image.NewNRGBA(image.Rect(0, 0, meta.width, meta.height))
		size := meta.completeSize
		if size <= 0 || size > len(raw) {
			size = len(raw)
		}
		if _, err := dds.DecompressDXT1(decoded.Pix, bytes.NewReader(raw[:size]), meta.width, meta.height, dds.Info{ColorModel: color.NRGBAModel}); err != nil {
			return nil, err
		}
		return unityFlipVerticalNRGBA(decoded), nil
	case uni.DXT1Crunched, uni.DXT5Crunched:
		// Unity's own fork of Rich Geldreich's crunch format: an additional compression layer on
		// top of plain DXT1/DXT5 block data (codebooks + Huffman, not a block format itself -- no
		// pure-Go decoder exists anywhere, every other Unity-asset tool wraps the same native
		// library). raw here is the whole crunch-compressed blob, not sized DXT block data, so it
		// skips the completeSize truncation the other cases use -- crunch.Decode consumes the
		// entire blob itself and returns exactly the right amount of real DXTn block bytes.
		dxtBytes, err := crunch.Decode(raw, 0)
		if err != nil {
			return nil, fmt.Errorf("crunch decode: %w", err)
		}
		decoded := image.NewNRGBA(image.Rect(0, 0, meta.width, meta.height))
		var decompressErr error
		if meta.format == uni.DXT1Crunched {
			_, decompressErr = dds.DecompressDXT1(decoded.Pix, bytes.NewReader(dxtBytes), meta.width, meta.height, dds.Info{ColorModel: color.NRGBAModel})
		} else {
			_, decompressErr = dds.DecompressDXT5(decoded.Pix, bytes.NewReader(dxtBytes), meta.width, meta.height, dds.Info{ColorModel: color.NRGBAModel})
		}
		if decompressErr != nil {
			return nil, decompressErr
		}
		return unityFlipVerticalNRGBA(decoded), nil
	default:
		textureCopy := *texture
		textureCopy.Width = int32(meta.width)
		textureCopy.Height = int32(meta.height)
		textureCopy.Format = meta.format
		textureCopy.ImageData = uni.NewResourceReader(uni.NewBinaryReaderFromBytes(raw, true), 0, int64(len(raw)))
		return uni.DecodeTexture2D(&textureCopy)
	}
}

type unityTextureMeta struct {
	width        int
	height       int
	completeSize int
	format       uni.TextureFormat
}

func unityNormalizedTextureMeta(texture *uni.Texture2D, rawLen int, hintWidth int, hintHeight int) unityTextureMeta {
	meta := unityTextureMeta{
		width:        int(texture.Width),
		height:       int(texture.Height),
		completeSize: int(texture.CompleteImageSize),
		format:       texture.Format,
	}

	// Dofus 3 image bundles currently expose a shifted field layout in ssm:
	// m_CompleteImageSize is read into height, and m_TextureFormat into mipsStripped.
	if meta.format == uni.Alpha8 && meta.completeSize == 0 && texture.MipsStripped.IsSome() && meta.height > meta.width {
		meta.completeSize = meta.height
		meta.format = uni.TextureFormat(texture.MipsStripped.Unwrap())
	}

	if meta.completeSize <= 0 {
		meta.completeSize = rawLen
	}
	if hintWidth > 0 && hintHeight > 0 && meta.completeSize > 0 && hintWidth*hintHeight == meta.completeSize {
		meta.width = hintWidth
		meta.height = hintHeight
		return meta
	}
	if hintWidth > 0 && meta.completeSize > 0 && meta.completeSize%hintWidth == 0 {
		derivedHeight := meta.completeSize / hintWidth
		if hintHeight <= 0 || absInt(derivedHeight-hintHeight) <= 2 {
			meta.width = hintWidth
			meta.height = derivedHeight
			return meta
		}
	}
	if hintHeight > 0 && meta.completeSize > 0 && meta.completeSize%hintHeight == 0 {
		derivedWidth := meta.completeSize / hintHeight
		if hintWidth <= 0 || absInt(derivedWidth-hintWidth) <= 2 {
			meta.width = derivedWidth
			meta.height = hintHeight
			return meta
		}
	}

	if meta.width > 0 {
		switch meta.format {
		case uni.BC7:
			if meta.completeSize%meta.width == 0 {
				meta.height = meta.completeSize / meta.width
			}
		case uni.Alpha8:
			if rawLen%meta.width == 0 {
				meta.height = rawLen / meta.width
			}
		case uni.RGB24:
			if rawLen%(meta.width*3) == 0 {
				meta.height = rawLen / (meta.width * 3)
			}
		case uni.RGBA32, uni.ARGB32:
			if rawLen%(meta.width*4) == 0 {
				meta.height = rawLen / (meta.width * 4)
			}
		}
	}

	return meta
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func unityFlipVerticalNRGBA(src *image.NRGBA) *image.NRGBA {
	if src == nil {
		return nil
	}
	bounds := src.Bounds()
	out := image.NewNRGBA(bounds)
	rowSize := bounds.Dx() * 4
	for y := 0; y < bounds.Dy(); y++ {
		srcStart := (bounds.Dy()-1-y)*src.Stride + bounds.Min.X*4
		dstStart := y*out.Stride + bounds.Min.X*4
		copy(out.Pix[dstStart:dstStart+rowSize], src.Pix[srcStart:srcStart+rowSize])
	}
	return out
}

func unityReadTextureData(texture *uni.Texture2D) ([]byte, error) {
	if texture == nil || texture.ImageData == nil {
		return nil, fmt.Errorf("texture has no image data reader")
	}

	resourceReader := texture.ImageData
	reader := resourceReader.GetReader()
	if reader == nil {
		return nil, fmt.Errorf("texture resource reader is unavailable")
	}

	size := resourceReader.Size
	if size <= 0 {
		return nil, nil
	}
	offset := resourceReader.Offset
	if offset < 0 || offset+size > reader.Len() {
		return nil, fmt.Errorf("texture data range is out of bounds (offset=%d size=%d len=%d)", offset, size, reader.Len())
	}
	if size > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("texture data size %d is too large", size)
	}

	if err := reader.SeekTo(offset); err != nil {
		return nil, err
	}
	out := reader.Bytes(int(size))
	return append([]byte(nil), out...), nil
}

func unityImageResolutionSubdir(bundleName string) string {
	base := strings.TrimSuffix(bundleName, ".imagebundle")
	lastUnderscore := strings.LastIndex(base, "_")
	if lastUnderscore < 0 || lastUnderscore+1 >= len(base) {
		return ""
	}

	// Real Unity resolution-variant suffixes are single-digit multipliers (1x/2x/4x/8x, e.g.
	// "item_assets_1"). The naive "any trailing digit run" version of this check misread
	// numeric asset IDs as resolution tags -- "prop_10008" produced a bogus "10008x" subdirectory
	// -- because a multi-digit ID looks identical to a resolution tag under a plain isOnlyDigits
	// test. Restricting to the four known multipliers fixes that without needing to enumerate
	// every category's naming convention.
	resolutionID := base[lastUnderscore+1:]
	switch resolutionID {
	case "1", "2", "4", "8":
		return resolutionID + "x"
	default:
		return ""
	}
}

func unityOutputImageName(assetName string, fallback string) string {
	name := strings.TrimSpace(assetName)
	if name == "" {
		name = fallback
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.TrimSpace(name)
	if name == "" {
		return "unnamed"
	}

	invalid := `<>:"/\|?*`
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(invalid, r) {
			return '_'
		}
		return r
	}, name)

	return name
}

func unityObjectFallbackName(object *uni.Object) string {
	if object == nil {
		return "unnamed"
	}
	return fmt.Sprintf("%d", object.PathID)
}

type unityNameState struct {
	total int
	byDim map[string]int
}

func unityBundleLayerDirs(bundleName string) []string {
	if strings.Contains(bundleName, "emblem_images_") {
		// For emblem bundles, the primary sprite is "up", with optional additional layers.
		return []string{"up", "backcontent", "outlinealliance", "outlineguild"}
	}
	return nil
}

func unityNextImagePath(dir string, baseName string, img image.Image, states map[string]*unityNameState, layerOrder []string) string {
	dimKey := "unknown"
	if img != nil {
		bounds := img.Bounds()
		dimKey = fmt.Sprintf("%dx%d", bounds.Dx(), bounds.Dy())
	}

	state, ok := states[baseName]
	if !ok {
		state = &unityNameState{byDim: make(map[string]int)}
		states[baseName] = state
	}

	outDir := dir
	if len(layerOrder) > 0 {
		outDir = filepath.Join(dir, layerOrder[state.total%len(layerOrder)])
	}

	basePath := filepath.Join(outDir, baseName+".png")
	if state.total == 0 {
		state.total++
		state.byDim[dimKey]++
		return basePath
	}

	state.total++
	state.byDim[dimKey]++

	var preferred string
	if state.byDim[dimKey] > 1 {
		// Same-dimension duplicates often represent truncated numeric IDs in AssetStudio output.
		suffix := state.byDim[dimKey] - 1
		if isOnlyDigits(baseName) && baseName != "0" && suffix == 1 {
			preferred = filepath.Join(outDir, fmt.Sprintf("%s_#%02d.png", baseName, suffix))
		} else {
			preferred = filepath.Join(outDir, fmt.Sprintf("%s_#%d.png", baseName, suffix))
		}
	} else {
		// Cross-dimension duplicates should collapse back to the same base name after cleanImages.
		preferred = filepath.Join(outDir, fmt.Sprintf("%s_#%02d.png", baseName, state.total-1))
	}
	if _, err := os.Stat(preferred); os.IsNotExist(err) {
		return preferred
	}

	for i := 1; ; i++ {
		candidate := filepath.Join(outDir, fmt.Sprintf("%s_#%02d_%d.png", baseName, state.total-1, i))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func unityWritePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return err
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	return png.Encode(file, img)
}
