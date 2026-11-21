package tree

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
)

// BlobKey returns the git blob-style OID (sha1 of "blob <n>\x00" + content), so the key of a file read from disk matches the OID in the index
// when the content is normalized to be the same as the blob.
func BlobKey(b []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(b)) + "\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
