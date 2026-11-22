// bgraph's Node collector (docs/dynamic-layer.md §8.2). Loaded with NODE_OPTIONS=--require into every Node process and thread of one
// test unit's run. It logs, per process and thread, the repository files the code loads, reads, lists or stats (with the first
// caller frame that is not Node-internal or a traversal helper), native addons, WASI instances, spawned processes and worker
// threads. The Go side (collect.go) merges the logs and decides what is a dependency and what makes the record incomplete.
//
// Configuration: config.json next to this file ({"root": <repository root, realpath>, "out": <log directory>}), not the
// environment, so that threads started with another env still find it.
'use strict';
const fs = require('node:fs');
const path = require('node:path');

let cfg = null;
try {
  cfg = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.json'), 'utf8'));
} catch {}

if (cfg && !globalThis.__bgraphCollect) {
  globalThis.__bgraphCollect = true;
  try {
    collect(cfg);
  } catch (e) {
    try {
      fs.appendFileSync(path.join(cfg.out, `${process.pid}-error.jsonl`), JSON.stringify({ ev: 'error', msg: String(e && e.stack || e) }) + '\n');
    } catch {}
  }
}

function collect(cfg) {
  const fsp = require('node:fs/promises');
  const Module = require('node:module');
  const cp = require('node:child_process');
  const wt = require('node:worker_threads');
  const { fileURLToPath, pathToFileURL } = require('node:url');

  const win = process.platform === 'win32';
  const fold = (s) => (win ? s.toLowerCase() : s);
  const ROOT = cfg.root;
  const rootF = fold(ROOT) + path.sep;
  const SEP_NM = path.sep + 'node_modules' + path.sep;
  const SELF = __filename;
  const PRELOAD = cfg.preload || ('--require=' + SELF); // exactly as it appears in NODE_OPTIONS
  const TOKEN_VAR = 'BGRAPH_COLLECT_PARENT';

  // Originals, captured before patching.
  const openSync = fs.openSync, writeSync = fs.writeSync, realpathNative = fs.realpathSync.native;
  let fd = -1;
  const logFile = path.join(cfg.out, `${process.pid}-${wt.threadId}.jsonl`);
  const emit = (o) => {
    try {
      if (fd < 0) fd = openSync(logFile, 'a');
      writeSync(fd, JSON.stringify(o) + '\n');
    } catch {}
  };
  emit({ ev: 'start', pid: process.pid, ppid: process.ppid, tid: wt.threadId, parent: process.env[TOKEN_VAR] || '',
    argv: process.argv.slice(1, 4) });

  const helpers = /[\\/]node_modules[\\/](?:\.pnpm[\\/][^\\/]+[\\/]node_modules[\\/])?(?:graceful-fs|fdir|tinyglobby|fast-glob|glob|globby|readdirp|picomatch|micromatch)[\\/]/;
  // caller: the first stack frame that is neither Node-internal, this file, nor a traversal helper ("" when none).
  // haste: a frame of jest-haste-map (its crawl reads every file for its import index).
  const caller = () => {
    const lim = Error.stackTraceLimit;
    Error.stackTraceLimit = 50;
    const st = new Error().stack;
    Error.stackTraceLimit = lim;
    let first = '', haste = false;
    for (const line of st.split('\n').slice(1)) {
      const m = /\(?((?:file:\/\/)?(?:[A-Za-z]:[\\/]|\/)[^()]*?):\d+:\d+\)?\s*$/.exec(line);
      if (!m) continue;
      let f = m[1];
      if (f.startsWith('file:')) {
        try { f = fileURLToPath(f); } catch { continue; }
      }
      if (f === SELF) continue;
      if (f.includes('jest-haste-map')) haste = true;
      if (!first && !helpers.test(f)) first = f;
    }
    return { caller: first, haste };
  };

  const rp = new Map();
  // repoPath: the realpath of p when it is inside the repository and outside node_modules, else null.
  const repoPath = (p) => {
    if (typeof p === 'number' || p == null) return null;
    try {
      if (p instanceof URL) p = fileURLToPath(p);
      else if (Buffer.isBuffer(p)) p = p.toString();
      else if (typeof p !== 'string') return null;
      if (p.startsWith('file:')) p = fileURLToPath(p);
    } catch { return null; }
    const a = path.resolve(p);
    if (!fold(a).startsWith(rootF)) return null;
    let r = rp.get(a);
    if (r === undefined) {
      try { r = realpathNative(a); } catch { r = a; }
      rp.set(a, r);
    }
    if (!fold(r).startsWith(rootF)) return null;
    if (r.includes(SEP_NM)) {
      // Vite's optimized dependencies: bundled natively and cached across runs (§8.3 rule 5).
      if (/[\\/]\.vite[\\/].*deps/.test(r)) return { cache: r };
      return null;
    }
    return r;
  };
  const seen = new Set();
  const rec = (ev, p, withCaller) => {
    const r = repoPath(p);
    if (!r) return;
    if (typeof r === 'object') {
      const k = 'cache\0' + r.cache;
      if (!seen.has(k)) { seen.add(k); emit({ ev: 'cache', path: r.cache }); }
      return;
    }
    let o = { ev, path: r };
    let k = ev + '\0' + r;
    if (withCaller) {
      const c = caller();
      o.caller = c.caller;
      if (c.haste) o.haste = true;
      k += '\0' + c.caller + '\0' + c.haste;
    }
    if (seen.has(k)) return;
    seen.add(k);
    emit(o);
  };
  const wrap = (obj, name, ev) => {
    const orig = obj && obj[name];
    if (typeof orig !== 'function') return;
    const w = { [name](p, ...rest) { rec(ev, p, true); return orig.call(this, p, ...rest); } }[name];
    Object.defineProperties(w, Object.getOwnPropertyDescriptors(orig));
    obj[name] = w;
  };
  for (const n of ['readFileSync', 'readFile', 'openSync', 'open', 'createReadStream', 'copyFileSync', 'copyFile', 'cpSync', 'cp',
    'openAsBlob', 'writeFileSync', 'writeFile', 'appendFileSync', 'appendFile', 'createWriteStream']) wrap(fs, n, 'read');
  for (const n of ['readFile', 'open', 'copyFile', 'cp', 'writeFile', 'appendFile']) wrap(fsp, n, 'read');
  for (const n of ['readdirSync', 'readdir', 'opendirSync', 'opendir']) wrap(fs, n, 'list');
  for (const n of ['readdir', 'opendir']) wrap(fsp, n, 'list');
  for (const n of ['statSync', 'stat', 'lstatSync', 'lstat']) wrap(fs, n, 'stat');
  for (const n of ['stat', 'lstat']) wrap(fsp, n, 'stat');
  for (const [obj, n] of [[fs, 'globSync'], [fs, 'glob'], [fsp, 'glob']]) {
    const orig = obj[n];
    if (typeof orig !== 'function') continue;
    obj[n] = function (pattern, ...rest) {
      const opts = rest.find((x) => x && typeof x === 'object') || {};
      const c = caller();
      emit({ ev: 'glob', cwd: String(opts.cwd || process.cwd()), caller: c.caller });
      return orig.call(this, pattern, ...rest);
    };
  }

  // Native addons.
  const dlopen = process.dlopen;
  process.dlopen = function (m, f, ...rest) {
    let r = String(f);
    try { r = realpathNative(r); } catch {}
    emit({ ev: 'native', path: r });
    return dlopen.call(this, m, f, ...rest);
  };
  try {
    const wasi = require('node:wasi');
    const W = wasi.WASI;
    wasi.WASI = class extends W {
      constructor(o) {
        super(o);
        const dirs = Object.values((o && o.preopens) || {}).map((d) => path.resolve(String(d)));
        emit({ ev: 'wasi', dirs });
      }
    };
  } catch {}

  // Processes: every spawn carries a token the child's start event reports.
  let nspawn = 0;
  const token = () => `${process.pid}-${wt.threadId}-${++nspawn}`;
  const withPreload = (v) => (v && v.includes(PRELOAD) ? v : ((v ? v + ' ' : '') + PRELOAD));
  const spawn = cp.ChildProcess.prototype.spawn;
  cp.ChildProcess.prototype.spawn = function (opts) {
    const tok = token();
    if (opts && Array.isArray(opts.envPairs)) {
      let no = '';
      const pairs = opts.envPairs.filter((e) => {
        if (e.startsWith(TOKEN_VAR + '=')) return false;
        if (e.startsWith('NODE_OPTIONS=')) { no = e.slice(13); return false; }
        return true;
      });
      opts.envPairs = pairs.concat(TOKEN_VAR + '=' + tok, 'NODE_OPTIONS=' + withPreload(no));
    }
    const c = caller();
    const r = spawn.call(this, opts);
    emit({ ev: 'spawn', token: tok, pid: this.pid || 0, file: String(opts && opts.file), args: ((opts && opts.args) || []).slice(1, 8).map(String),
      caller: c.caller });
    return r;
  };
  for (const n of ['spawnSync', 'execSync', 'execFileSync']) {
    const orig = cp[n];
    if (typeof orig !== 'function') continue;
    cp[n] = function (file, ...rest) {
      const tok = token();
      let i = rest.findIndex((x) => x && typeof x === 'object' && !Array.isArray(x));
      if (i < 0) {
        // execSync(cmd), spawnSync(file), spawnSync(file, args), execFileSync(file, args): options go last.
        rest.push({});
        i = rest.length - 1;
      }
      const o = { ...rest[i] };
      const env = { ...(o.env || process.env) };
      env[TOKEN_VAR] = tok;
      env.NODE_OPTIONS = withPreload(env.NODE_OPTIONS);
      o.env = env;
      rest[i] = o;
      const args = Array.isArray(rest[0]) ? rest[0].slice(0, 7).map(String) : [];
      emit({ ev: 'spawn', token: tok, sync: true, file: String(file), args, caller: caller().caller });
      return orig.call(this, file, ...rest);
    };
  }

  // Worker threads: a plain-object env gets the preload; every construction is logged with the new thread id.
  const Worker = wt.Worker;
  wt.Worker = class extends Worker {
    constructor(file, opts) {
      if (opts && opts.env && typeof opts.env === 'object') {
        opts = { ...opts, env: { ...opts.env, NODE_OPTIONS: withPreload(opts.env.NODE_OPTIONS) } };
      }
      super(file, opts);
      emit({ ev: 'worker', tid: this.threadId, caller: caller().caller });
    }
  };
  Module.syncBuiltinESMExports();

  // Module loads: CommonJS through Module.prototype.load, ESM through a module.register load hook (hooks.mjs).
  const load = Module.prototype.load;
  Module.prototype.load = function (filename) {
    rec('load', filename, false);
    return load.call(this, filename);
  };
  if (typeof Module.register === 'function') {
    try {
      Module.register(pathToFileURL(path.join(__dirname, 'bgraph_hooks.mjs')), {
        data: { root: ROOT, file: path.join(cfg.out, `${process.pid}-${wt.threadId}-hooks.jsonl`) },
      });
    } catch (e) {
      emit({ ev: 'nohooks', msg: String(e && e.message) });
    }
  } else {
    emit({ ev: 'nohooks', msg: 'module.register is missing (Node < 20.6)' });
  }
}
