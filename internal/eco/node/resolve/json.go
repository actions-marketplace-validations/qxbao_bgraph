package resolve

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Kind là loại của một giá trị JSON có thứ tự khoá.
type Kind int

const (
	KNull Kind = iota
	KString
	KArray
	KObject
	KOther // số, bool
)

// JSONValue là cây JSON giữ nguyên THỨ TỰ khoá của object (cần cho exports/imports).
type JSONValue struct {
	Kind Kind
	Str  string
	Arr  []*JSONValue
	Keys []string     // chỉ với KObject, theo thứ tự xuất hiện
	Vals []*JSONValue // song song với Keys
}

// Get trả giá trị của khoá (khoá cuối cùng nếu trùng); nil nếu không phải object hoặc không có.
func (v *JSONValue) Get(key string) *JSONValue {
	if v == nil || v.Kind != KObject {
		return nil
	}
	for i := len(v.Keys) - 1; i >= 0; i-- {
		if v.Keys[i] == key {
			return v.Vals[i]
		}
	}
	return nil
}

// AsString trả chuỗi nếu v là string.
func (v *JSONValue) AsString() (string, bool) {
	if v != nil && v.Kind == KString {
		return v.Str, true
	}
	return "", false
}

// ParseJSONC đọc JSON có chú thích (// và /* */) và dấu phẩy thừa.
func ParseJSONC(data []byte) (*JSONValue, error) {
	dec := json.NewDecoder(bytes.NewReader(stripJSONC(data)))
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("dữ liệu thừa sau giá trị JSON")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (*JSONValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &JSONValue{Kind: KObject}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("khoá object không phải chuỗi")
				}
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				v.Keys = append(v.Keys, k)
				v.Vals = append(v.Vals, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return v, nil
		case '[':
			v := &JSONValue{Kind: KArray}
			for dec.More() {
				val, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				v.Arr = append(v.Arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return v, nil
		}
		return nil, errors.New("dấu phân cách JSON bất ngờ")
	case string:
		return &JSONValue{Kind: KString, Str: t}, nil
	case nil:
		return &JSONValue{Kind: KNull}, nil
	default:
		return &JSONValue{Kind: KOther}, nil
	}
}

// skipTrivia bỏ khoảng trắng và chú thích từ vị trí i, trả vị trí ký tự có nghĩa kế tiếp.
func skipTrivia(b []byte, i int) int {
	for i < len(b) {
		c := b[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			j := bytes.Index(b[i+2:], []byte("*/"))
			if j < 0 {
				return len(b)
			}
			i += 2 + j + 2
		default:
			return i
		}
	}
	return i
}

// stripJSONC xoá BOM, chú thích và dấu phẩy thừa (trước } hoặc ]) ngoài chuỗi.
func stripJSONC(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(b) && b[j] != '"' {
				if b[j] == '\\' {
					j++
				}
				j++
			}
			end := min(j+1, len(b))
			out = append(out, b[i:end]...)
			i = end
		case c == '/' && i+1 < len(b) && (b[i+1] == '/' || b[i+1] == '*'):
			j := skipTrivia(b, i)
			out = append(out, ' ')
			i = j
		case c == ',':
			k := skipTrivia(b, i+1)
			if k < len(b) && (b[k] == '}' || b[k] == ']') {
				i++ // bỏ dấu phẩy thừa
			} else {
				out = append(out, c)
				i++
			}
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}
