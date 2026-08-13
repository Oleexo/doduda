package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dofusdude/ankabuffer"
)

// matchFragmentFiles resolves a --file pattern against one fragment's file map. A literal pattern
// must equal a manifest key exactly (mirrors the non-regex branch of DownloadUnpackFiles,
// update.go:743-748). A "REGEX:"-prefixed pattern is compiled with the standard library regexp
// package and matched against every key, same convention as the "REGEX:" HashFile.Filename entries
// already used by images.go (e.g. the worldmap category). Files whose manifest Name is empty are
// skipped either way -- that's how ankabuffer marks a file as not present on this platform/release.
func matchFragmentFiles(fragment ankabuffer.Fragment, pattern string) ([]string, error) {
	if after, ok := strings.CutPrefix(pattern, "REGEX:"); ok {
		compiled, err := regexp.Compile(after)
		if err != nil {
			return nil, fmt.Errorf("invalid --file regex %q: %w", after, err)
		}

		var matched []string
		for key, file := range fragment.Files {
			if file.Name == "" {
				continue
			}
			if compiled.MatchString(key) {
				matched = append(matched, key)
			}
		}
		sort.Strings(matched)
		return matched, nil
	}

	file, ok := fragment.Files[pattern]
	if !ok || file.Name == "" {
		return nil, nil
	}
	return []string{pattern}, nil
}

func resolveFragment(manifest *ankabuffer.Manifest, fragment string) (ankabuffer.Fragment, error) {
	frag, ok := manifest.Fragments[fragment]
	if !ok {
		names := make([]string, 0, len(manifest.Fragments))
		for name := range manifest.Fragments {
			names = append(names, name)
		}
		sort.Strings(names)
		return ankabuffer.Fragment{}, fmt.Errorf("unknown fragment %q, available: %s", fragment, strings.Join(names, ", "))
	}
	return frag, nil
}

// ListFragments prints every fragment in the manifest with its file count and total size. Reads
// only the already-loaded manifest -- no network access.
func ListFragments(manifest *ankabuffer.Manifest) error {
	names := make([]string, 0, len(manifest.Fragments))
	for name := range manifest.Fragments {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		frag := manifest.Fragments[name]
		var total int64
		count := 0
		for _, file := range frag.Files {
			if file.Name == "" {
				continue
			}
			total += file.Size
			count++
		}
		fmt.Printf("%-20s %6d files  %10s\n", name, count, humanFileSize(float64(total), true, 1))
	}
	return nil
}

// ListFiles prints every file in one fragment matching pattern, with its size. Reads only the
// already-loaded manifest -- no network access.
func ListFiles(manifest *ankabuffer.Manifest, fragment string, pattern string) error {
	frag, err := resolveFragment(manifest, fragment)
	if err != nil {
		return err
	}

	matched, err := matchFragmentFiles(frag, pattern)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		return fmt.Errorf("no files in fragment %q matched %q", fragment, pattern)
	}

	for _, key := range matched {
		fmt.Printf("%-100s %10s\n", key, humanFileSize(float64(frag.Files[key].Size), true, 1))
	}
	return nil
}

// countOutputFiles walks dir and counts regular files, used by Extract to detect the silent
// zero-output case (a bundle with no Sprite/Texture2D for --as image, or not exactly one
// MonoBehaviour for --as data -- unpackUnityImageBundleNative and unpackUnityBundleNative both
// return nil in those cases instead of an error).
func countOutputFiles(dir string) int {
	count := 0
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// buildExtractHashFiles turns matched manifest keys into HashFiles with a friendly name that
// selects the right decoder on write (Unpack, update.go:586 dispatches on the written file's
// extension) and survives DownloadUnpackFiles' parallel per-file goroutines without colliding.
//
// Existing categories share one FriendlyName across every REGEX match in a category (e.g. the
// worldmap category, images.go:496) because doduda knows in advance those files are safe to merge
// unpacking-wise. Extract doesn't have that guarantee for an arbitrary pattern, so each match gets
// its own name derived from its own base name, deduplicated on collision.
//
// For --as data, the name must end ".asset.bundle", not just ".bundle": UnpackUnityBundle
// (update.go:551-552) trims a literal ".asset.json" suffix off the computed output path before
// re-appending ".json" -- every hand-written data category already relies on this (see the
// "<x>.asset.bundle" -> "<x>.json" pattern throughout items.go). Skipping the ".asset" infix here
// would leave a stray ".json.json" wart on the requested Filename's basename.
func buildExtractHashFiles(matched []string, as string) []HashFile {
	var ext string
	if as == "data" {
		ext = ".asset.bundle"
	} else {
		ext = ".imagebundle"
	}

	used := make(map[string]int, len(matched))
	files := make([]HashFile, 0, len(matched))
	for _, key := range matched {
		base := strings.TrimSuffix(filepath.Base(key), filepath.Ext(key))
		friendly := base + ext
		if n, seen := used[friendly]; seen {
			n++
			used[friendly] = n
			friendly = fmt.Sprintf("%s_%d%s", base, n, ext)
		} else {
			used[friendly] = 0
		}
		files = append(files, HashFile{Filename: key, FriendlyName: friendly})
	}
	return files
}

// Extract downloads every file in fragment matching pattern and unpacks it with the existing
// native decoder -- UnpackUnityImages (Sprite/Texture2D -> PNG) for as == "image", or
// UnpackUnityBundle (single MonoBehaviour -> JSON typetree) for as == "data". It's the generic
// escape hatch alongside the hand-written categories in images.go/items.go/etc: any fragment, any
// bundle, no code change or rebuild needed to try a new one.
func Extract(manifest *ankabuffer.Manifest, fragment string, pattern string, outDir string, as string, bin int, headless bool) error {
	if as != "image" && as != "data" {
		return fmt.Errorf("--as must be \"image\" or \"data\", got %q", as)
	}

	frag, err := resolveFragment(manifest, fragment)
	if err != nil {
		return err
	}

	matched, err := matchFragmentFiles(frag, pattern)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		return fmt.Errorf("no files in fragment %q matched %q", fragment, pattern)
	}

	if err := os.MkdirAll(outDir, os.ModePerm); err != nil {
		return err
	}

	before := countOutputFiles(outDir)

	files := buildExtractHashFiles(matched, as)
	title := fmt.Sprintf("Extract %s/%s", fragment, pattern)
	if err := DownloadUnpackFiles(title, bin, manifest, fragment, files, outDir, outDir, true, "", headless, false); err != nil {
		return err
	}

	after := countOutputFiles(outDir)
	if after <= before {
		return fmt.Errorf(
			"extraction produced no new files under %s -- the %d matched bundle(s) likely contain neither Sprite nor Texture2D objects (--as image), or not exactly one MonoBehaviour (--as data)",
			outDir, len(matched))
	}

	fmt.Printf("✅ extracted %d bundle(s) → %d output file(s) under %s\n", len(files), after-before, outDir)
	return nil
}
