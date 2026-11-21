package resolve

import "strings"

// Tên built-in của Node 22–26 (module.builtinModules, không kèm tiền tố node:).
var builtinNames = map[string]bool{
	"assert": true, "async_hooks": true, "buffer": true, "child_process": true, "cluster": true,
	"console": true, "constants": true, "crypto": true, "dgram": true, "diagnostics_channel": true,
	"dns": true, "domain": true, "events": true, "fs": true, "http": true, "http2": true, "https": true,
	"inspector": true, "module": true, "net": true, "os": true, "path": true, "perf_hooks": true,
	"process": true, "punycode": true, "querystring": true, "readline": true, "repl": true,
	"stream": true, "string_decoder": true, "sys": true, "timers": true, "tls": true,
	"trace_events": true, "tty": true, "url": true, "util": true, "v8": true, "vm": true,
	"wasi": true, "worker_threads": true, "zlib": true,
}

var builtinSubs = map[string]bool{
	"promises": true, "posix": true, "win32": true, "web": true, "consumers": true, "strict": true, "types": true,
}

// isBuiltin: spec có phải module lõi của Node không. Tiền tố node: luôn là built-in
// (node:test, node:sqlite... chỉ tồn tại với tiền tố).
func isBuiltin(spec string) bool {
	if strings.HasPrefix(spec, "node:") {
		return true
	}
	name, sub, found := strings.Cut(spec, "/")
	if !builtinNames[name] {
		return false
	}
	return !found || builtinSubs[sub]
}
