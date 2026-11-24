package godyn

// Line diff, as in internal/dynamic/phpdyn and jvmdyn (the layers share no helper package).

type hunk struct{ b0, b1, h0, h1 int } // base lines [b0,b1), head lines [h0,h1), 0-based

// diffHunks computes the changed regions between a and b (Myers' O(ND) algorithm after trimming the common prefix and suffix).
// When the edit distance is very large the middle is one hunk (wider, never narrower) and exact is false: its line counts do not
// tell whether lines inside it moved.
func diffHunks(a, b []string) (hunks []hunk, exact bool) {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	a2, b2 := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if len(a2) == 0 && len(b2) == 0 {
		return nil, true
	}
	whole := []hunk{{pre, pre + len(a2), pre, pre + len(b2)}}
	if len(a2) == 0 || len(b2) == 0 {
		return whole, true
	}
	ids := map[string]int{}
	id := func(s string) int {
		if v, ok := ids[s]; ok {
			return v
		}
		ids[s] = len(ids)
		return len(ids) - 1
	}
	x := make([]int, len(a2))
	y := make([]int, len(b2))
	for i, s := range a2 {
		x[i] = id(s)
	}
	for i, s := range b2 {
		y[i] = id(s)
	}
	n, m := len(x), len(y)
	maxD := n + m
	if maxD > 1000 {
		maxD = 1000
	}
	off := maxD + 1
	v := make([]int, 2*maxD+3)
	var trace [][]int
	found := -1
	for d := 0; d <= maxD && found < 0; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var i int
			if k == -d || k != d && v[off+k-1] < v[off+k+1] {
				i = v[off+k+1]
			} else {
				i = v[off+k-1] + 1
			}
			j := i - k
			for i < n && j < m && x[i] == y[j] {
				i++
				j++
			}
			v[off+k] = i
			if i >= n && j >= m {
				found = d
				break
			}
		}
	}
	if found < 0 {
		return whole, false
	}
	// Backtrack: mark matched (diagonal) pairs.
	type pair struct{ i, j int }
	var diag []pair
	i, j := n, m
	for d := found; d > 0; d-- {
		vv := trace[d]
		k := i - j
		var pk int
		if k == -d || k != d && vv[off+k-1] < vv[off+k+1] {
			pk = k + 1
		} else {
			pk = k - 1
		}
		pi := vv[off+pk]
		pj := pi - pk
		mi := pi + 1 // a deletion from a (move right)
		if pk == k+1 {
			mi = pi // an insertion from b (move down)
		}
		for i > mi {
			diag = append(diag, pair{i - 1, j - 1})
			i--
			j--
		}
		i, j = pi, pj
	}
	for i > 0 && j > 0 {
		diag = append(diag, pair{i - 1, j - 1})
		i--
		j--
	}
	// diag is in reverse order; walk forward collecting gaps.
	var out []hunk
	ci, cj := 0, 0
	for k := len(diag) - 1; k >= -1; k-- {
		ni, nj := n, m
		if k >= 0 {
			ni, nj = diag[k].i, diag[k].j
		}
		if ni > ci || nj > cj {
			out = append(out, hunk{pre + ci, pre + ni, pre + cj, pre + nj})
		}
		ci, cj = ni+1, nj+1
	}
	return out, true
}
