package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"golang.org/x/text/unicode/norm"
)

// DerivedName is the drag-and-drop address: <under>/<hex sha256(NFC(filename))>.
// It is a TOTAL function of any real filename — spaces, unicode, emoji,
// 200-byte titles — so no upload can fail on its name, while the same
// filename always derives the same name, which is what turns a re-upload into
// a REPLACE (the name repoints; the CAS keeps the old bytes as history).
// NFC normalisation is applied before hashing so a file dragged from a macOS
// Finder (NFD) and the same title typed on Linux (NFC) address one blob. The
// filename itself is stored verbatim (not normalised) as metadata.
func DerivedName(under, filename string) (string, error) {
	if err := ValidName(under); err != nil {
		return "", fmt.Errorf("under: %w", err)
	}
	if err := ValidFilename(filename); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(norm.NFC.String(filename)))
	name := under + "/" + hex.EncodeToString(sum[:])
	if len(name) > MaxNameBytes {
		return "", fmt.Errorf("derived name exceeds %d bytes (under %q is too long)", MaxNameBytes, under)
	}
	return name, nil
}
