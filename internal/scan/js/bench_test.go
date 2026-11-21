package js

import (
	"fmt"
	"strings"
	"testing"
)

// synthTS dựng tệp TS ~1 MB gồm import, lớp, generic, template, regex, chia, comment, require/mock.
func synthTS(size int) []byte {
	var b strings.Builder
	i := 0
	for b.Len() < size {
		b.WriteString(strings.ReplaceAll(fmt.Sprintf(`import { Foo%[1]d, type Bar%[1]d, baz as qux%[1]d } from './mod%[1]d';
import type { T%[1]d } from "../types/t%[1]d";
// comment about module %[1]d with 'quotes' and import x from 'fake'
/**
 * Doc block %[1]d: require('fake')
 */
export interface Props%[1]d<T extends Record<string, number>> { id: T; name?: string; cb(x: number): void }
export class Service%[1]d<T = unknown> extends Base<Map<string, T>> {
  private readonly cache = new Map<string, Array<T>>();
  constructor(private readonly dep: Dep%[1]d, @Inject(TOKEN) public tok: string) { super(); }
  async load(id: string, n = 10): Promise<T | undefined> {
    const total = (n + %[1]d) / 2 / 3;
    if (!/^[a-z0-9\/]+$/i.test(id)) { throw new Error(BTbad id ${id} at ${this.name ?? 'none'}BT); }
    const m = await import('./lazy%[1]d');
    const r = require('./cjs%[1]d');
    return this.cache.get(id) as T | undefined;
  }
}
export const helper%[1]d = <T,>(x: T, ...rest: number[]): T => { return x; };
const obj%[1]d = { a: 1, b: [1, 2, 3], c: "it's", d: 0xFF + 1_000, e: x!.y?.z };
`, i), "BT", "`"))
		if i%7 == 0 {
			fmt.Fprintf(&b, "vi.mock('./dep%d', () => ({ default: 1 }));\n", i)
		}
		i++
	}
	return []byte(b.String())
}

func BenchmarkScan(b *testing.B) {
	src := synthTS(1 << 20)
	if r := Scan("a.ts", src); r.Uncertain {
		b.Fatalf("synthetic source uncertain: %s", r.UncertainReason)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Scan("a.ts", src)
	}
}

func TestSynthCertain(t *testing.T) {
	r := Scan("a.ts", synthTS(64<<10))
	if r.Uncertain {
		t.Fatal(r.UncertainReason)
	}
	if len(r.Imports) == 0 {
		t.Fatal("no imports")
	}
}
