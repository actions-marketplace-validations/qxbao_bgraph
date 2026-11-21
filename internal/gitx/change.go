package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
)

// Status là trạng thái một tệp đổi so với base: 'A' thêm, 'M' sửa, 'D' xoá, 'T' đổi kiểu (tệp ↔ symlink ↔ gitlink).
type Status byte

// ChangedFile là một tệp đổi. Mode ở dạng git ("100644", "100755", "120000", "160000", "000000" = không có).
type ChangedFile struct {
	Path    string
	Status  Status
	OldMode string
	NewMode string
}

// Change là tập thay đổi giữa base và cây làm việc.
//
// Gitlink (submodule, mode 160000) xuất hiện trong Files với mode để người gọi nới rộng. Thư mục untracked là repo git
// lồng nhau (ls-files báo "dir/") xuất hiện với Path đã bỏ dấu "/" cuối, Status 'A' và NewMode "160000".
type Change struct {
	Base  string // OID commit đã phân giải
	Files []ChangedFile
}

// Snapshot chụp tập thay đổi của cây làm việc so với base và trả cây làm việc. Chạy song song đúng 3 tiến trình git.
// base nên là OID đầy đủ (từ RevParse); nếu không phải thì Snapshot tự phân giải (thêm một spawn).
func (r *Repo) Snapshot(ctx context.Context, base string) (Change, *WorkTree, error) {
	if !isHexOID(base) {
		oid, err := r.RevParse(ctx, base)
		if err != nil {
			return Change{}, nil, err
		}
		base = oid
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var diffOut, lsOut, idxOut []byte
	var errs [3]error
	var wg sync.WaitGroup
	run := func(i int, out *[]byte, args ...string) {
		defer wg.Done()
		*out, errs[i] = r.run(ctx, args...)
		if errs[i] != nil {
			cancel()
		}
	}
	wg.Add(3)
	go run(0, &diffOut, "diff", "--raw", "-z", "--no-renames", "--no-ext-diff", "--ignore-submodules=none", base, "--")
	go run(1, &lsOut, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=none", "--no-renames")
	go run(2, &idxOut, "ls-files", "-z", "-s")
	wg.Wait()
	// Báo lỗi gốc, không phải "context canceled" do cancel() dây chuyền.
	for _, e := range errs {
		if e != nil && !errors.Is(e, context.Canceled) {
			return Change{}, nil, e
		}
	}
	for _, e := range errs {
		if e != nil {
			return Change{}, nil, e
		}
	}

	diff, err := parseRaw(diffOut)
	if err != nil {
		return Change{}, nil, err
	}
	ls, err := parseStatusV2(lsOut)
	if err != nil {
		return Change{}, nil, err
	}
	idx, err := parseIndex(idxOut)
	if err != nil {
		return Change{}, nil, err
	}

	files := map[string]ChangedFile{}
	for _, f := range diff {
		addChange(files, f)
	}
	for _, p := range ls.untracked {
		if strings.HasSuffix(p, "/") {
			addChange(files, ChangedFile{Path: strings.TrimRight(p, "/"), Status: 'A', OldMode: "000000", NewMode: "160000"})
		} else {
			addChange(files, ChangedFile{Path: p, Status: 'A', OldMode: "000000", NewMode: "100644"})
		}
	}
	ch := Change{Base: base, Files: make([]ChangedFile, 0, len(files))}
	for _, f := range files {
		ch.Files = append(ch.Files, f)
	}
	slices.SortFunc(ch.Files, func(a, b ChangedFile) int { return strings.Compare(a.Path, b.Path) })

	wt := buildWorkTree(r.Root, idx, ls, diff)
	return ch, wt, nil
}

// addChange gộp bản ghi trùng đường dẫn (ví dụ 'U' + 'M' của tệp xung đột, hoặc 'D' do `git rm --cached` + untracked
// vẫn còn trên đĩa): mode cũ lấy từ phía có, mode mới lấy từ phía có, trạng thái suy lại.
func addChange(m map[string]ChangedFile, f ChangedFile) {
	old, ok := m[f.Path]
	if !ok {
		m[f.Path] = f
		return
	}
	if isZeroMode(old.OldMode) {
		old.OldMode = f.OldMode
	}
	if isZeroMode(old.NewMode) {
		old.NewMode = f.NewMode
	}
	switch {
	case isZeroMode(old.OldMode):
		old.Status = 'A'
	case isZeroMode(old.NewMode):
		old.Status = 'D'
	case modeType(old.OldMode) != modeType(old.NewMode):
		old.Status = 'T'
	default:
		old.Status = 'M'
	}
	m[f.Path] = old
}

func isZeroMode(m string) bool { return m == "" || m == "000000" }

func modeType(m string) string {
	if len(m) >= 3 {
		return m[:3] // 100 = tệp, 120 = symlink, 160 = gitlink
	}
	return m
}

// parseRaw parse đầu ra `git diff --raw -z`: ":<om> <nm> <ooid> <noid> <status>\0<path>\0" (rename/copy có hai đường dẫn).
func parseRaw(b []byte) ([]ChangedFile, error) {
	var out []ChangedFile
	toks := bytes.Split(b, []byte{0})
	if n := len(toks); n > 0 && len(toks[n-1]) == 0 {
		toks = toks[:n-1]
	}
	for i := 0; i < len(toks); {
		meta := string(toks[i])
		i++
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("gitx: diff --raw: bản ghi không hiểu được %q", meta)
		}
		f := strings.Fields(meta[1:])
		if len(f) != 5 || f[4] == "" {
			return nil, fmt.Errorf("gitx: diff --raw: siêu dữ liệu sai dạng %q", meta)
		}
		st := f[4][0]
		npaths := 1
		if st == 'R' || st == 'C' {
			npaths = 2
		}
		if i+npaths > len(toks) {
			return nil, fmt.Errorf("gitx: diff --raw: thiếu đường dẫn sau %q", meta)
		}
		paths := make([]string, npaths)
		for k := range paths {
			paths[k] = string(toks[i+k])
		}
		i += npaths
		om, nm := f[0], f[1]
		switch st {
		case 'A', 'M', 'D', 'T':
			out = append(out, ChangedFile{Path: paths[0], Status: Status(st), OldMode: om, NewMode: nm})
		case 'U':
			// Chưa merge: coi là sửa; addChange gộp với bản ghi khác của cùng đường dẫn nếu có.
			out = append(out, ChangedFile{Path: paths[0], Status: 'M', OldMode: om, NewMode: nm})
		case 'R': // phòng thủ: --no-renames đáng lẽ không bao giờ sinh ra
			out = append(out,
				ChangedFile{Path: paths[0], Status: 'D', OldMode: om, NewMode: "000000"},
				ChangedFile{Path: paths[1], Status: 'A', OldMode: "000000", NewMode: nm})
		case 'C':
			out = append(out, ChangedFile{Path: paths[1], Status: 'A', OldMode: "000000", NewMode: nm})
		default:
			return nil, fmt.Errorf("gitx: diff --raw: trạng thái không biết %q cho %q", f[4], paths[0])
		}
	}
	return out, nil
}

type lsFiles struct {
	modified  map[string]bool // Y != '.' (cây làm việc khác index) hoặc chưa merge
	removed   map[string]bool // Y == 'D' (xoá khỏi cây làm việc)
	untracked []string        // '?' (thư mục repo lồng có "/" cuối)
}

// parseStatusV2 parse `git status --porcelain=v2 -z`. Bản ghi (đường dẫn có thể chứa dấu cách nên cắt có giới hạn):
//
//	1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
//	2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <Xscore> <path>\0<origPath>
//	u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
//	? <path>   |   ! <path>   |   # <header>
//
// Bản ghi loại 2 (rename/copy trong index) được xử lý thận trọng: cả path và origPath đều bị coi là đã sửa.
func parseStatusV2(b []byte) (lsFiles, error) {
	res := lsFiles{modified: map[string]bool{}, removed: map[string]bool{}}
	toks := bytes.Split(b, []byte{0})
	if n := len(toks); n > 0 && len(toks[n-1]) == 0 {
		toks = toks[:n-1]
	}
	for i := 0; i < len(toks); i++ {
		t := string(toks[i])
		if t == "" {
			continue
		}
		bad := func() (lsFiles, error) {
			return res, fmt.Errorf("gitx: status v2: bản ghi không hiểu được %q", t)
		}
		switch t[0] {
		case '#', '!':
			// tiêu đề nhánh / tệp bị ignore: không ảnh hưởng
		case '?':
			if len(t) < 3 || t[1] != ' ' {
				return bad()
			}
			res.untracked = append(res.untracked, t[2:])
		case '1', '2', 'u':
			nf := map[byte]int{'1': 9, '2': 10, 'u': 11}[t[0]]
			f := strings.SplitN(t, " ", nf)
			if len(f) != nf || len(f[1]) != 2 {
				return bad()
			}
			p := f[nf-1]
			xy := f[1]
			if t[0] == 'u' {
				res.modified[p] = true
				break
			}
			if xy[1] != '.' {
				res.modified[p] = true
			}
			if xy[1] == 'D' {
				res.removed[p] = true
			}
			if t[0] == '2' {
				res.modified[p] = true
				i++
				if i >= len(toks) {
					return bad()
				}
				res.modified[string(toks[i])] = true // origPath
			}
		default:
			return bad()
		}
	}
	return res, nil
}

type indexEntry struct {
	mode, oid string
	stage     int
	path      string
}

// parseIndex parse `ls-files -z -s`: "<mode> <oid> <stage>\t<path>\0".
func parseIndex(b []byte) ([]indexEntry, error) {
	var out []indexEntry
	for _, t := range bytes.Split(b, []byte{0}) {
		if len(t) == 0 {
			continue
		}
		tab := bytes.IndexByte(t, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("gitx: ls-files -s: thiếu tab %q", t)
		}
		f := strings.Fields(string(t[:tab]))
		if len(f) != 3 || len(f[2]) != 1 || f[2][0] < '0' || f[2][0] > '3' {
			return nil, fmt.Errorf("gitx: ls-files -s: siêu dữ liệu sai dạng %q", t[:tab])
		}
		out = append(out, indexEntry{mode: f[0], oid: f[1], stage: int(f[2][0] - '0'), path: string(t[tab+1:])})
	}
	return out, nil
}

// lstatMissing: đường dẫn (tương đối gốc) không còn trên đĩa.
func lstatMissing(root, p string) bool {
	_, err := os.Lstat(joinRoot(root, p))
	return err != nil && os.IsNotExist(err)
}
