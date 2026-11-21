// Package rules đọc bgraph.toml (mục 6.7 của docs/plan.md): luật do người dùng khai và khai báo đầu ra sinh mã.
//
// Giải mã nghiêm ngặt: khoá lạ là lỗi. Lỗi bgraph.toml KHÔNG bao giờ được bỏ qua; bên gọi nới tới full (user_rule).
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qxbao/bgraph/internal/glob"

	"github.com/pelletier/go-toml/v2"
)

// FileName là tên tệp luật ở gốc repo.
const FileName = "bgraph.toml"

// DefaultMaxChanged là ngưỡng mặc định số tệp đổi; vượt thì chạy full.
const DefaultMaxChanged = 5000

// Rule là một [[rule]]. AffectsAll/AffectsNone/Affects loại trừ nhau.
type Rule struct {
	Index       int // thứ tự trong tệp, từ 0
	Paths       []*glob.Pattern
	RawPaths    []string
	AffectsAll  bool
	AffectsNone bool
	Affects     []string // thư mục dự án hoặc glob test
	Reason      string
}

// Generated là một [[generated]]: đầu ra bị ignore mà mã import.
type Generated struct {
	Outputs, Sources       []*glob.Pattern
	RawOutputs, RawSources []string
	Owner                  string
}

// Options là các núm tuỳ chọn.
type Options struct {
	MaxChanged int
}

// File là nội dung bgraph.toml đã kiểm tra.
type File struct {
	Rules     []Rule
	Generated []Generated
	Options   Options
	Path      string // đường dẫn tệp đã đọc; rỗng nếu không có tệp
}

type rawFile struct {
	Rule []struct {
		Paths   []string `toml:"paths"`
		Affects any      `toml:"affects"`
		Reason  string   `toml:"reason"`
	} `toml:"rule"`
	Generated []struct {
		Outputs []string `toml:"outputs"`
		Sources []string `toml:"sources"`
		Owner   string   `toml:"owner"`
	} `toml:"generated"`
	Options struct {
		MaxChanged *int64 `toml:"max_changed"`
	} `toml:"options"`
}

// Load đọc <root>/bgraph.toml. Không có tệp → File rỗng với mặc định, không lỗi.
func Load(root string) (*File, error) {
	p := filepath.Join(root, FileName)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &File{Options: Options{MaxChanged: DefaultMaxChanged}}, nil
		}
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return Parse(p, data)
}

// Parse giải mã và kiểm tra nội dung; name dùng trong thông báo lỗi và gán vào File.Path.
func Parse(name string, data []byte) (*File, error) {
	var raw rawFile
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var se *toml.StrictMissingError
		if errors.As(err, &se) {
			return nil, fmt.Errorf("%s: khoá không hợp lệ:\n%s", name, se.String())
		}
		var de *toml.DecodeError
		if errors.As(err, &de) {
			return nil, fmt.Errorf("%s: %s", name, de.String())
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	f := &File{Path: name, Options: Options{MaxChanged: DefaultMaxChanged}}

	for i, r := range raw.Rule {
		out := Rule{Index: i, RawPaths: r.Paths, Reason: r.Reason}
		if len(r.Paths) == 0 {
			return nil, fmt.Errorf("%s: rule[%d]: 'paths' bắt buộc và không được rỗng", name, i)
		}
		for _, s := range r.Paths {
			p, err := glob.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("%s: rule[%d].paths: %w", name, i, err)
			}
			out.Paths = append(out.Paths, p)
		}
		switch v := r.Affects.(type) {
		case nil:
			return nil, fmt.Errorf("%s: rule[%d]: 'affects' bắt buộc", name, i)
		case string:
			switch v {
			case "all":
				out.AffectsAll = true
			case "none":
				out.AffectsNone = true
			default:
				return nil, fmt.Errorf("%s: rule[%d].affects: chuỗi phải là \"all\" hoặc \"none\" (nhận %q); dùng mảng cho thư mục/glob", name, i, v)
			}
		case []any:
			if len(v) == 0 {
				return nil, fmt.Errorf("%s: rule[%d].affects: mảng rỗng", name, i)
			}
			for _, e := range v {
				s, ok := e.(string)
				if !ok || s == "" {
					return nil, fmt.Errorf("%s: rule[%d].affects: phần tử phải là chuỗi không rỗng", name, i)
				}
				if _, err := glob.Compile(s); err != nil {
					return nil, fmt.Errorf("%s: rule[%d].affects: %w", name, i, err)
				}
				out.Affects = append(out.Affects, s)
			}
		default:
			return nil, fmt.Errorf("%s: rule[%d].affects: phải là \"all\", \"none\" hoặc mảng chuỗi", name, i)
		}
		f.Rules = append(f.Rules, out)
	}

	for i, g := range raw.Generated {
		out := Generated{RawOutputs: g.Outputs, RawSources: g.Sources, Owner: g.Owner}
		if len(g.Outputs) == 0 {
			return nil, fmt.Errorf("%s: generated[%d]: 'outputs' bắt buộc và không được rỗng", name, i)
		}
		if g.Owner == "" {
			return nil, fmt.Errorf("%s: generated[%d]: 'owner' bắt buộc", name, i)
		}
		for _, s := range g.Outputs {
			p, err := glob.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("%s: generated[%d].outputs: %w", name, i, err)
			}
			out.Outputs = append(out.Outputs, p)
		}
		for _, s := range g.Sources {
			p, err := glob.Compile(s)
			if err != nil {
				return nil, fmt.Errorf("%s: generated[%d].sources: %w", name, i, err)
			}
			out.Sources = append(out.Sources, p)
		}
		f.Generated = append(f.Generated, out)
	}

	if m := raw.Options.MaxChanged; m != nil {
		if *m < 1 {
			return nil, fmt.Errorf("%s: options.max_changed: phải ≥ 1 (nhận %d)", name, *m)
		}
		f.Options.MaxChanged = int(*m)
	}
	return f, nil
}

// Match trả các luật có paths khớp path, theo thứ tự trong tệp.
func (f *File) Match(path string) []Rule {
	var out []Rule
	for _, r := range f.Rules {
		for _, p := range r.Paths {
			if p.Match(path) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// GeneratedFor trả [[generated]] đầu tiên có outputs khớp path, hoặc nil.
func (f *File) GeneratedFor(path string) *Generated {
	for i := range f.Generated {
		for _, p := range f.Generated[i].Outputs {
			if p.Match(path) {
				return &f.Generated[i]
			}
		}
	}
	return nil
}
