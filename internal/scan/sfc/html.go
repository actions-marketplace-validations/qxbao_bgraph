package sfc

import (
	"bytes"
	"strings"
)

type attr struct {
	name string // đã hạ chữ thường
	val  string
	has  bool // có "=giá trị"
}

type tag struct {
	name  string // chữ thường
	attrs []attr
	pos   int // vị trí '<'
	end   int // vị trí sau '>'
	self  bool
}

func (t *tag) get(name string) (string, bool) {
	for _, a := range t.attrs {
		if a.name == name {
			return a.val, true
		}
	}
	return "", false
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' }

// delim: ký tự sau tên thẻ phải là khoảng trắng, '>' hoặc '/' (hoặc hết tệp).
func delim(src []byte, j int) bool {
	return j >= len(src) || isSpace(src[j]) || src[j] == '>' || src[j] == '/'
}

// skipBraces bỏ qua {...} cân bằng (thuộc tính Svelte/Astro), bỏ qua chuỗi bên trong. Trả vị trí sau '}' hoặc -1.
func skipBraces(src []byte, j int) int {
	depth := 0
	for j < len(src) {
		switch c := src[j]; c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j + 1
			}
		case '"', '\'', '`':
			k := j + 1
			for k < len(src) && src[k] != c {
				if src[k] == '\\' {
					k++
				}
				k++
			}
			j = k
		}
		j++
	}
	return -1
}

// parseTag đọc thẻ mở tại src[i]=='<'. ok=false: thẻ không đóng được.
func parseTag(src []byte, i int) (t tag, ok bool) {
	n := len(src)
	j := i + 1
	s := j
	for j < n && !isSpace(src[j]) && src[j] != '>' && src[j] != '/' {
		j++
	}
	t = tag{name: string(asciiLower(src[s:j])), pos: i}
	for {
		for j < n && isSpace(src[j]) {
			j++
		}
		if j >= n {
			return t, false
		}
		c := src[j]
		if c == '>' {
			t.end = j + 1
			return t, true
		}
		if c == '/' {
			if j+1 < n && src[j+1] == '>' {
				t.self = true
				t.end = j + 2
				return t, true
			}
			j++
			continue
		}
		s = j
		if c == '{' { // {...spread} / {shorthand}
			k := skipBraces(src, j)
			if k < 0 {
				return t, false
			}
			j = k
			continue
		}
		for j < n && !isSpace(src[j]) && src[j] != '=' && src[j] != '>' && src[j] != '/' {
			j++
		}
		a := attr{name: string(asciiLower(src[s:j]))}
		if s == j {
			j++
			continue
		}
		for j < n && isSpace(src[j]) {
			j++
		}
		if j < n && src[j] == '=' {
			a.has = true
			j++
			for j < n && isSpace(src[j]) {
				j++
			}
			if j >= n {
				return t, false
			}
			switch q := src[j]; {
			case q == '"' || q == '\'':
				k := bytes.IndexByte(src[j+1:], q)
				if k < 0 {
					return t, false
				}
				a.val = string(src[j+1 : j+1+k])
				j += k + 2
			case q == '{':
				k := skipBraces(src, j)
				if k < 0 {
					return t, false
				}
				a.val = string(src[j:k])
				j = k
			default:
				s = j
				for j < n && !isSpace(src[j]) && src[j] != '>' {
					j++
				}
				a.val = string(src[s:j])
			}
		}
		t.attrs = append(t.attrs, a)
	}
}

// findClose tìm thẻ đóng "</name" + delim từ vị trí from (lsrc là bản hạ chữ thường). Trả (vị trí '<', vị trí sau '>').
func findClose(lsrc []byte, from int, name string) (int, int, bool) {
	pat := []byte("</" + name)
	k := from
	for {
		x := bytes.Index(lsrc[k:], pat)
		if x < 0 {
			return 0, 0, false
		}
		p := k + x
		if delim(lsrc, p+len(pat)) {
			g := bytes.IndexByte(lsrc[p:], '>')
			if g < 0 {
				return 0, 0, false
			}
			return p, p + g + 1, true
		}
		k = p + 1
	}
}

// skipComment: tại "<!--" trả vị trí sau "-->"; ok=false nếu không đóng.
func skipComment(src []byte, i int) (int, bool) {
	x := bytes.Index(src[i+4:], []byte("-->"))
	if x < 0 {
		return len(src), false
	}
	return i + 4 + x + 3, true
}

func startsAt(src []byte, i int, p string) bool {
	return i+len(p) <= len(src) && string(src[i:i+len(p)]) == p
}

// langExt đổi thuộc tính lang của script thành đuôi ảo cho bộ quét JS. ok=false: lang không hỗ trợ.
func langExt(lang string) (string, bool) {
	switch strings.ToLower(lang) {
	case "", "js", "javascript", "mjs", "cjs":
		return ".js", true
	case "jsx":
		return ".jsx", true
	case "ts", "typescript", "mts", "cts":
		return ".ts", true
	case "tsx":
		return ".tsx", true
	}
	return "", false
}

// styleExt: lang của style → dạng cú pháp cho scanCSS. ok=false: không hỗ trợ (stylus...).
func styleExt(lang string) (string, bool) {
	switch strings.ToLower(lang) {
	case "", "css", "postcss", "pcss":
		return "css", true
	case "scss":
		return "scss", true
	case "sass":
		return "sass", true
	case "less":
		return "less", true
	}
	return "", false
}

// jsType: giá trị thuộc tính type của script có phải mã JS/TS không (rỗng = có).
func jsType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "module", "text/javascript", "application/javascript", "text/ecmascript", "application/ecmascript",
		"text/typescript", "application/typescript", "ts", "text/jsx", "text/babel":
		return true
	}
	return false
}

// ---- Vue ----

func scanVue(a *acc) {
	src := a.src
	lsrc := asciiLower(src)
	n := len(src)
	seenScript, seenSetup := false, false
	i := 0
	for i < n && !a.stopped() {
		x := bytes.IndexByte(src[i:], '<')
		if x < 0 {
			return
		}
		i += x
		if startsAt(src, i, "<!--") {
			e, ok := skipComment(src, i)
			if !ok {
				a.fail("vue", "unclosed_comment: top-level <!-- at line "+itoa(a.line(i)))
				return
			}
			i = e
			continue
		}
		if startsAt(src, i, "</") {
			// thẻ đóng lạc ở mức cao nhất của script/style/template: không chắc ranh giới khối
			for _, nm := range []string{"script", "style", "template"} {
				if startsAt(lsrc, i, "</"+nm) && delim(lsrc, i+2+len(nm)) {
					a.fail("vue", "stray_close_tag: </"+nm+"> at line "+itoa(a.line(i)))
					return
				}
			}
			i += 2
			continue
		}
		if i+1 >= n || !(lsrc[i+1] >= 'a' && lsrc[i+1] <= 'z') {
			i++
			continue
		}
		t, ok := parseTag(src, i)
		if !ok {
			a.fail("vue", "unclosed_tag: <"+t.name+"> at line "+itoa(a.line(i)))
			return
		}
		tl := a.line(i)
		kind := t.name
		srcAttr, hasSrc := t.get("src")
		_, hasSetup := t.get("setup")
		if t.name == "script" {
			if hasSetup {
				kind = "script setup"
				if seenSetup {
					a.fail(kind, "duplicate_script_setup: second <script setup> at line "+itoa(tl))
					return
				}
				seenSetup = true
			} else {
				if seenScript {
					a.fail(kind, "duplicate_script: second <script> at line "+itoa(tl))
					return
				}
				seenScript = true
			}
		}
		if hasSrc && srcAttr != "" && !remote(srcAttr) {
			a.ref(srcAttr, tl)
		}
		if t.self {
			i = t.end
			continue
		}
		var cs, ce, end int
		if t.name == "template" {
			// template chứa HTML lồng nhau: đếm độ sâu <template>/</template>, bỏ qua comment
			depth, j := 1, t.end
			found := false
			for j < n {
				x := bytes.IndexByte(src[j:], '<')
				if x < 0 {
					break
				}
				j += x
				if startsAt(src, j, "<!--") {
					e, ok := skipComment(src, j)
					if !ok {
						break
					}
					j = e
					continue
				}
				if startsAt(lsrc, j, "</template") && delim(lsrc, j+10) {
					depth--
					g := bytes.IndexByte(src[j:], '>')
					if g < 0 {
						break
					}
					if depth == 0 {
						cs, ce, end, found = t.end, j, j+g+1, true
						break
					}
					j += g + 1
					continue
				}
				if startsAt(lsrc, j, "<template") && delim(lsrc, j+9) {
					nt, ok := parseTag(src, j)
					if !ok {
						break
					}
					if !nt.self {
						depth++
					}
					j = nt.end
					continue
				}
				j++
			}
			if !found {
				a.fail(kind, "unclosed_block: <template> at line "+itoa(tl))
				return
			}
		} else {
			p, e, ok := findClose(lsrc, t.end, t.name)
			if !ok {
				a.fail(kind, "unclosed_block: <"+t.name+"> at line "+itoa(tl))
				return
			}
			cs, ce, end = t.end, p, e
		}
		content := src[cs:ce]
		switch t.name {
		case "script":
			lang, _ := t.get("lang")
			ext, ok := langExt(lang)
			if !ok {
				a.fail(kind, "script_lang: unsupported lang="+lang)
				return
			}
			a.scanJS(kind, content, ext, cs)
		case "style":
			lang, _ := t.get("lang")
			ext, ok := styleExt(lang)
			if !ok {
				a.fail(kind, "style_lang: unsupported lang="+lang)
				return
			}
			a.scanStyle(kind, ext, content, cs)
		}
		i = end
	}
}

// ---- quét "mọi nơi" dùng cho Svelte và Astro ----

// scanLoose duyệt phần thân từ vị trí from, tìm mọi <script>/<style> (ở bất kỳ độ sâu nào, bỏ qua comment HTML).
// Thừa còn hơn sót: script lồng trong phần tử cũng được quét.
// style: tách riêng vì Svelte/Astro đều xử lý @import trong <style>.
func scanLoose(a *acc, from int, label string, astro bool) {
	src := a.src
	lsrc := asciiLower(src)
	n := len(src)
	i := from
	for i < n && !a.stopped() {
		x := bytes.IndexByte(src[i:], '<')
		if x < 0 {
			return
		}
		i += x
		if startsAt(src, i, "<!--") {
			e, ok := skipComment(src, i)
			if !ok {
				a.fail(label, "unclosed_comment: <!-- at line "+itoa(a.line(i)))
				return
			}
			i = e
			continue
		}
		var name string
		for _, nm := range []string{"script", "style"} {
			if startsAt(lsrc, i, "<"+nm) && delim(lsrc, i+1+len(nm)) {
				name = nm
			} else if startsAt(lsrc, i, "</"+nm) && delim(lsrc, i+2+len(nm)) {
				a.fail(label, "stray_close_tag: </"+nm+"> at line "+itoa(a.line(i)))
				return
			}
		}
		if name == "" {
			i++
			continue
		}
		t, ok := parseTag(src, i)
		if !ok {
			a.fail(label, "unclosed_tag: <"+name+"> at line "+itoa(a.line(i)))
			return
		}
		tl := a.line(i)
		kind := name
		srcAttr, hasSrc := t.get("src")
		if name == "script" && hasSrc && relative(srcAttr) {
			a.ref(srcAttr, tl)
		}
		if t.self {
			i = t.end
			continue
		}
		p, e, ok := findClose(lsrc, t.end, name)
		if !ok {
			a.fail(kind, "unclosed_block: <"+name+"> at line "+itoa(tl))
			return
		}
		content := src[t.end:p]
		i = e
		switch name {
		case "script":
			typ, _ := t.get("type")
			if !jsType(typ) {
				continue // JSON-LD, importmap, template...: không phải mã module
			}
			if astro {
				if _, inline := t.get("is:inline"); inline || hasSrc {
					continue // không được bundle: chỉ lấy src (đã xử lý ở trên)
				}
				a.scanJS(kind, content, ".ts", t.end)
				continue
			}
			if hasSrc {
				continue // Svelte không xử lý script có src
			}
			lang, _ := t.get("lang")
			ext, ok := langExt(lang)
			if !ok {
				a.fail(kind, "script_lang: unsupported lang="+lang)
				return
			}
			a.scanJS(kind, content, ext, t.end)
		case "style":
			lang, _ := t.get("lang")
			ext, ok := styleExt(lang)
			if !ok {
				a.fail(kind, "style_lang: unsupported lang="+lang)
				return
			}
			a.scanStyle(kind, ext, content, t.end)
		}
	}
}

func scanSvelte(a *acc) { scanLoose(a, 0, "svelte", false) }

// scanAstro: frontmatter (TS) giữa hai dòng "---" ở đầu tệp, rồi thân.
func scanAstro(a *acc) {
	src := a.src
	n := len(src)
	p := 0
	for p < n && isSpace(src[p]) {
		p++
	}
	body := 0
	if startsAt(src, p, "---") {
		le := bytes.IndexByte(src[p:], '\n')
		if le < 0 {
			le = n - p
		}
		if len(bytes.TrimSpace(src[p:p+le])) == 3 { // dòng mở đúng là "---"
			cs := p + le + 1
			if cs > n {
				cs = n
			}
			j := cs
			found := false
			for j <= n {
				e := bytes.IndexByte(src[j:], '\n')
				end := n
				if e >= 0 {
					end = j + e
				}
				if string(bytes.TrimSpace(src[j:end])) == "---" {
					a.scanJS("frontmatter", src[cs:j], ".ts", cs)
					body = end
					found = true
					break
				}
				if e < 0 {
					break
				}
				j = end + 1
			}
			if !found {
				a.fail("frontmatter", "unclosed_frontmatter: no closing --- for frontmatter opened at line "+itoa(a.line(p)))
				return
			}
		}
	}
	if a.stopped() {
		return
	}
	scanLoose(a, body, "astro", true)
}
