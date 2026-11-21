package tree

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
)

// BlobKey trả OID kiểu blob của git (sha1 "blob <n>\x00" + nội dung), để khoá của tệp đọc từ đĩa trùng với OID trong index
// khi nội dung đã chuẩn hoá giống blob.
func BlobKey(b []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(b)) + "\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
