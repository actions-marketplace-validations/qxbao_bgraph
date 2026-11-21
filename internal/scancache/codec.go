package scancache

import (
	"encoding/binary"
	"errors"

	"github.com/qxbao/bgraph/internal/model"
)

// Compact binary encoding for ScanResult (measured: smaller and faster to decode than JSON, see the benchmark in store_test.go).
//
// Layout: [ver=1][flags: bit0 uncertain][uvarint len+bytes UncertainReason][uvarint import count]
// per import: [kind byte][flags: bit0 dynamic][uvarint line][str spec][str suffix]; str = uvarint len + bytes.
// Kind is an index into the kinds table; 255 = an unknown string follows (forward compatibility when new kinds are added).

const codecVersion = 1

var kinds = []model.ImportKind{
	model.ImportStatic, model.ImportTypeOnly, model.ImportDynamic, model.ImportRequire, model.ImportResolve,
	model.ImportMock, model.ImportGlob, model.ImportURL, model.ImportReExport, model.ImportTripleSlash,
}

var kindIdx = func() map[model.ImportKind]byte {
	m := map[model.ImportKind]byte{}
	for i, k := range kinds {
		m[k] = byte(i)
	}
	return m
}()

func appendStr(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func encode(r model.ScanResult) []byte {
	b := make([]byte, 0, 16+len(r.Imports)*24)
	b = append(b, codecVersion)
	var fl byte
	if r.Uncertain {
		fl = 1
	}
	b = append(b, fl)
	b = appendStr(b, r.UncertainReason)
	b = binary.AppendUvarint(b, uint64(len(r.Imports)))
	for _, im := range r.Imports {
		if i, ok := kindIdx[im.Kind]; ok {
			b = append(b, i)
		} else {
			b = append(b, 255)
			b = appendStr(b, string(im.Kind))
		}
		var f byte
		if im.Dynamic {
			f = 1
		}
		b = append(b, f)
		b = binary.AppendUvarint(b, uint64(im.Line))
		b = appendStr(b, im.Spec)
		b = appendStr(b, im.Suffix)
	}
	return b
}

var errCodec = errors.New("scancache: corrupt encoded data")

type reader struct {
	b   []byte
	err error
}

func (r *reader) byte() byte {
	if len(r.b) == 0 {
		r.err = errCodec
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *reader) uvarint() uint64 {
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errCodec
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) str() string {
	n := r.uvarint()
	if r.err != nil || n > uint64(len(r.b)) {
		r.err = errCodec
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

func decode(b []byte) (model.ScanResult, error) {
	r := &reader{b: b}
	var out model.ScanResult
	if r.byte() != codecVersion {
		return out, errCodec
	}
	out.Uncertain = r.byte()&1 != 0
	out.UncertainReason = r.str()
	n := r.uvarint()
	if r.err != nil || n > uint64(len(b)) { // each import takes ≥ 5 bytes, so n > len means corruption
		return model.ScanResult{}, errCodec
	}
	if n > 0 {
		out.Imports = make([]model.ImportRef, 0, n)
	}
	for i := uint64(0); i < n; i++ {
		var im model.ImportRef
		k := r.byte()
		if k == 255 {
			im.Kind = model.ImportKind(r.str())
		} else if int(k) < len(kinds) {
			im.Kind = kinds[k]
		} else {
			return model.ScanResult{}, errCodec
		}
		im.Dynamic = r.byte()&1 != 0
		im.Line = int(r.uvarint())
		im.Spec = r.str()
		im.Suffix = r.str()
		if r.err != nil {
			return model.ScanResult{}, r.err
		}
		out.Imports = append(out.Imports, im)
	}
	return out, nil
}
