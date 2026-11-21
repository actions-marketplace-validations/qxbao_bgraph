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

// Task is a normalised T1 task.
type Task struct {
	InstanceID string          `json:"instance_id"`
	Repo       string          `json:"repo"`
	BaseCommit string          `json:"base_commit"`
	Patch      string          `json:"patch"`
	TestPatch  string          `json:"test_patch"`
	F2P        json.RawMessage `json:"FAIL_TO_PASS"`
	Source     string          `json:"source,omitempty"`
}

// ParseF2P reads FAIL_TO_PASS: a JSON array, a string containing a JSON array, or a Python list-literal-style string ('a', "b").
func ParseF2P(raw json.RawMessage) ([]string, error) {
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("FAIL_TO_PASS is not a list/string: %w", err)
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
		return nil, fmt.Errorf("not a list literal: %.40q", s)
	}
	i++
	out := []string{}
	for {
		skip()
		if i >= len(r) {
			return nil, fmt.Errorf("unterminated list")
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
			return nil, fmt.Errorf("element at %d is not a string", i)
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
			return nil, fmt.Errorf("unterminated string")
		}
		i++
		out = append(out, b.String())
	}
}

// LoadTasks reads JSONL.
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

// Manifest is the list of repos of one ecosystem with dev/holdout roles (section 10.2: split by REPO).
type Manifest struct {
	Ecosystem string         `json:"ecosystem"`
	Version   int            `json:"version"`
	SplitRule string         `json:"split_rule"`
	CapPer    int            `json:"t1_cap_per_repo"`
	Datasets  []DatasetInfo  `json:"datasets"`
	Repos     []ManifestRepo `json:"repos"`
}

// DatasetInfo describes the data source.
type DatasetInfo struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Revision string `json:"revision,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// ManifestRepo is one repo in the manifest.
type ManifestRepo struct {
	Name    string   `json:"name"`
	Role    string   `json:"role"` // dev | holdout
	T1      int      `json:"t1_tasks"`
	Replay  bool     `json:"replay"`
	Sources []string `json:"sources,omitempty"`
}

// SplitRule describes the dev/holdout split rule.
const SplitRule = "sort repos (lowercased) by ascending sha256(name); index i%3==2 (every 3rd repo) is holdout, the rest dev"

// BuildManifest builds a deterministic manifest from a set of repos (union of the T1 repos and the replay repos).
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

// Role returns the repo's role (case-insensitive match).
func (m Manifest) Role(repo string) (string, bool) {
	for _, r := range m.Repos {
		if strings.EqualFold(r.Name, repo) {
			return r.Role, true
		}
	}
	return "", false
}

// LoadManifest reads the manifest.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}
