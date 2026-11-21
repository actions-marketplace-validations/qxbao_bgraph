package evalkit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Task là một nhiệm vụ T1 đã chuẩn hoá.
type Task struct {
	InstanceID string          `json:"instance_id"`
	Repo       string          `json:"repo"`
	BaseCommit string          `json:"base_commit"`
	Patch      string          `json:"patch"`
	TestPatch  string          `json:"test_patch"`
	F2P        json.RawMessage `json:"FAIL_TO_PASS"`
	Source     string          `json:"source,omitempty"`
}

// ParseF2P đọc FAIL_TO_PASS: mảng JSON, chuỗi chứa mảng JSON, hoặc chuỗi kiểu literal list của Python ('a', "b").
func ParseF2P(raw json.RawMessage) ([]string, error) {
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("FAIL_TO_PASS không phải list/chuỗi: %w", err)
	}
	s = strings.TrimSpace(s)
	if err := json.Unmarshal([]byte(s), &arr); err == nil {
		return arr, nil
	}
	return parsePyList(s)
}

func parsePyList(s string) ([]string, error) {
	r := []rune(s)
	i := 0
	skip := func() {
		for i < len(r) && (r[i] == ' ' || r[i] == '\n' || r[i] == '\t' || r[i] == '\r') {
			i++
		}
	}
	skip()
	if i >= len(r) || r[i] != '[' {
		return nil, fmt.Errorf("không phải literal list: %.40q", s)
	}
	i++
	out := []string{}
	for {
		skip()
		if i >= len(r) {
			return nil, fmt.Errorf("list không đóng")
		}
		if r[i] == ']' {
			return out, nil
		}
		if r[i] == ',' {
			i++
			continue
		}
		q := r[i]
		if q != '\'' && q != '"' {
			return nil, fmt.Errorf("phần tử không phải chuỗi tại %d", i)
		}
		i++
		var b strings.Builder
		for i < len(r) && r[i] != q {
			if r[i] == '\\' && i+1 < len(r) {
				i++
				switch r[i] {
				case 'n':
					b.WriteRune('\n')
				case 't':
					b.WriteRune('\t')
				case 'r':
					b.WriteRune('\r')
				case 'u', 'x':
					n := 4
					if r[i] == 'x' {
						n = 2
					}
					if i+n < len(r) {
						var v rune
						if _, err := fmt.Sscanf(string(r[i+1:i+1+n]), "%x", &v); err == nil {
							b.WriteRune(v)
							i += n
							break
						}
					}
					b.WriteRune(r[i])
				default:
					b.WriteRune(r[i])
				}
				i++
				continue
			}
			b.WriteRune(r[i])
			i++
		}
		if i >= len(r) {
			return nil, fmt.Errorf("chuỗi không đóng")
		}
		i++
		out = append(out, b.String())
	}
}

// LoadTasks đọc JSONL.
func LoadTasks(path string) ([]Task, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Task
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var t Task
			if e := json.Unmarshal(line, &t); e != nil {
				return nil, fmt.Errorf("%s: %w", path, e)
			}
			out = append(out, t)
		}
		if err != nil {
			break
		}
	}
	return out, nil
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// Manifest là danh sách repo của một hệ sinh thái với vai trò dev/holdout (mục 10.2: tách theo REPO).
type Manifest struct {
	Ecosystem string         `json:"ecosystem"`
	Version   int            `json:"version"`
	SplitRule string         `json:"split_rule"`
	CapPer    int            `json:"t1_cap_per_repo"`
	Datasets  []DatasetInfo  `json:"datasets"`
	Repos     []ManifestRepo `json:"repos"`
}

// DatasetInfo mô tả nguồn dữ liệu.
type DatasetInfo struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Revision string `json:"revision,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// ManifestRepo là một repo trong manifest.
type ManifestRepo struct {
	Name    string   `json:"name"`
	Role    string   `json:"role"` // dev | holdout
	T1      int      `json:"t1_tasks"`
	Replay  bool     `json:"replay"`
	Sources []string `json:"sources,omitempty"`
}

// SplitRule mô tả luật chia dev/holdout.
const SplitRule = "sắp repo (chữ thường) theo sha256(tên) tăng dần; chỉ số i%3==2 (mỗi repo thứ 3) là holdout, còn lại dev"

// BuildManifest dựng manifest tất định từ tập repo (hợp của repo T1 và repo replay).
func BuildManifest(t1 map[string]*ManifestRepo, replay []string, ds []DatasetInfo, capPer int) Manifest {
	all := map[string]*ManifestRepo{}
	for k, v := range t1 {
		c := *v
		all[strings.ToLower(k)] = &c
	}
	for _, r := range replay {
		k := strings.ToLower(r)
		if all[k] == nil {
			all[k] = &ManifestRepo{Name: r}
		}
		all[k].Replay = true
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return sha(keys[i]) < sha(keys[j]) })
	m := Manifest{Ecosystem: "node", Version: 1, SplitRule: SplitRule, CapPer: capPer, Datasets: ds}
	for i, k := range keys {
		r := all[k]
		r.Role = "dev"
		if i%3 == 2 {
			r.Role = "holdout"
		}
		sort.Strings(r.Sources)
		m.Repos = append(m.Repos, *r)
	}
	return m
}

// Role trả vai trò của repo (so khớp không phân biệt hoa thường).
func (m Manifest) Role(repo string) (string, bool) {
	for _, r := range m.Repos {
		if strings.EqualFold(r.Name, repo) {
			return r.Role, true
		}
	}
	return "", false
}

// LoadManifest đọc manifest.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}
