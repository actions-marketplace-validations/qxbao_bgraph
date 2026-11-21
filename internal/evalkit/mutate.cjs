// Deterministic mutation generator for T2 (see internal/evalkit/t2.go). Run with node, reads one JSON argument:
//   {ts, repo, rev, include:[regex], exclude:[regex], count, seed}
// Prints to stdout a JSON array of mutations {file, line, op, start, end, repl, before, after, blob} (start/end are BYTE OFFSETS in the blob at rev).
// Mutations are based on the TypeScript AST (which also reads JS): each mutation edits exactly one node, inside a function body (except export renames),
// and is syntax-checked with transpileModule before being accepted. Selection round-robins over directories and operators, using a PRNG seeded from `seed`.
'use strict';
const cp = require('child_process');
const cfg = JSON.parse(process.argv[2].startsWith('@') ? require('fs').readFileSync(process.argv[2].slice(1), 'utf8') : process.argv[2]);
const ts = require(cfg.ts);

function git(args, opts) {
  return cp.execFileSync('git', ['-C', cfg.repo, ...args], { maxBuffer: 1 << 28, ...opts });
}
function mulberry32(a) {
  return function () {
    a |= 0; a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
function hash(s) { let h = 2166136261; for (const c of s) { h ^= c.charCodeAt(0); h = Math.imul(h, 16777619); } return h >>> 0; }
const rnd = mulberry32(hash(cfg.seed));
const pick = (a) => a[Math.floor(rnd() * a.length)];
function shuffle(a) { a = a.slice(); for (let i = a.length - 1; i > 0; i--) { const j = Math.floor(rnd() * (i + 1)); [a[i], a[j]] = [a[j], a[i]]; } return a; }

const inc = cfg.include.map((r) => new RegExp(r));
const exc = cfg.exclude.map((r) => new RegExp(r));
const files = git(['ls-tree', '-r', '--name-only', cfg.rev]).toString().split('\n').filter((f) => f && inc.some((r) => r.test(f)) && !exc.some((r) => r.test(f))).sort();

const SWAP = {
  [ts.SyntaxKind.PlusToken]: '-', [ts.SyntaxKind.MinusToken]: '+',
  [ts.SyntaxKind.LessThanToken]: '<=', [ts.SyntaxKind.LessThanEqualsToken]: '<',
  [ts.SyntaxKind.GreaterThanToken]: '>=', [ts.SyntaxKind.GreaterThanEqualsToken]: '>',
  [ts.SyntaxKind.EqualsEqualsEqualsToken]: '!==', [ts.SyntaxKind.ExclamationEqualsEqualsToken]: '===',
};
const isFn = (n) => ts.isFunctionDeclaration(n) || ts.isFunctionExpression(n) || ts.isArrowFunction(n) || ts.isMethodDeclaration(n) || ts.isConstructorDeclaration(n) || ts.isGetAccessor(n) || ts.isSetAccessor(n);
const hasExport = (n) => {
  const m = n.modifiers || [];
  return m.some((x) => x.kind === ts.SyntaxKind.ExportKeyword) && !m.some((x) => x.kind === ts.SyntaxKind.DefaultKeyword);
};

function sites(file) {
  const blob = git(['cat-file', 'blob', `${cfg.rev}:${file}`]);
  const text = blob.toString('utf8');
  if (text.includes('�')) return { blob, text, out: [] };
  const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
  const out = [];
  const add = (op, node, from, to, repl) => {
    const line = sf.getLineAndCharacterOfPosition(from).line + 1;
    out.push({ op, from, to, repl, line });
  };
  let depth = 0;
  (function walk(n) {
    const fn = isFn(n);
    if (fn) depth++;
    if (ts.isImportDeclaration(n) || ts.isTypeAliasDeclaration(n) || ts.isInterfaceDeclaration(n) || ts.isTypeNode(n)) { if (fn) depth--; return; }
    if (depth > 0) {
      if (ts.isIfStatement(n)) add('negate-cond', n, n.expression.getStart(sf), n.expression.end, '!(' + n.expression.getText(sf) + ')');
      if (ts.isConditionalExpression(n)) add('negate-cond', n, n.condition.getStart(sf), n.condition.end, '!(' + n.condition.getText(sf) + ')');
      if (ts.isBinaryExpression(n) && SWAP[n.operatorToken.kind]) add('swap-op', n, n.operatorToken.getStart(sf), n.operatorToken.end, SWAP[n.operatorToken.kind]);
      if (ts.isReturnStatement(n) && n.expression && n.expression.getText(sf) !== 'undefined') add('return-undefined', n, n.expression.getStart(sf), n.expression.end, 'undefined');
      if (ts.isExpressionStatement(n) && ts.isBlock(n.parent) && (ts.isCallExpression(n.expression) || ts.isBinaryExpression(n.expression) || ts.isAwaitExpression(n.expression)) &&
        !(ts.isCallExpression(n.expression) && n.expression.expression.kind === ts.SyntaxKind.SuperKeyword)) add('delete-stmt', n, n.getStart(sf), n.end, ';');
    }
    if (ts.isSourceFile(n.parent || n) || true) {
      let nameNode = null;
      if ((ts.isFunctionDeclaration(n) || ts.isClassDeclaration(n)) && n.name && hasExport(n)) nameNode = n.name;
      if (ts.isVariableStatement(n) && hasExport(n) && n.declarationList.declarations[0] && ts.isIdentifier(n.declarationList.declarations[0].name)) nameNode = n.declarationList.declarations[0].name;
      if (nameNode && depth === 0) add('rename-export', n, nameNode.getStart(sf), nameNode.end, nameNode.getText(sf) + '__mut');
    }
    ts.forEachChild(n, walk);
    if (fn) depth--;
  })(sf);
  return { blob, text, out };
}

function syntaxOK(file, src) {
  const r = ts.transpileModule(src, { fileName: file, reportDiagnostics: true, compilerOptions: { target: ts.ScriptTarget.ESNext, jsx: ts.JsxEmit.Preserve } });
  return !r.diagnostics || r.diagnostics.length === 0;
}

const cache = new Map();
const get = (f) => { if (!cache.has(f)) cache.set(f, sites(f)); return cache.get(f); };
const byDir = new Map();
for (const f of files) { const d = f.slice(0, f.lastIndexOf('/')); if (!byDir.has(d)) byDir.set(d, []); byDir.get(d).push(f); }
const dirs = shuffle([...byDir.keys()]);
const OPS = ['negate-cond', 'swap-op', 'return-undefined', 'delete-stmt', 'rename-export'];
const res = [], used = new Set();
for (let i = 0; res.length < cfg.count && i < cfg.count * 60; i++) {
  const op = OPS[i % OPS.length];
  const dir = dirs[(i + Math.floor(i / OPS.length)) % dirs.length];
  const cand = byDir.get(dir).filter((f) => get(f).out.some((s) => s.op === op && !used.has(f + ':' + s.line)));
  if (!cand.length) continue;
  const file = pick(cand);
  const { blob, text, out } = get(file);
  const s = pick(out.filter((x) => x.op === op && !used.has(file + ':' + x.line)));
  const mutated = text.slice(0, s.from) + s.repl + text.slice(s.to);
  if (!syntaxOK(file, mutated)) { used.add(file + ':' + s.line); continue; }
  used.add(file + ':' + s.line);
  const start = Buffer.byteLength(text.slice(0, s.from)), end = start + Buffer.byteLength(text.slice(s.from, s.to));
  const lines = text.split('\n');
  const before = lines[s.line - 1].trim();
  const afterLine = mutated.split('\n')[s.line - 1] || '';
  res.push({ file, line: s.line, op, start, end, repl: s.repl, before: before.slice(0, 200), after: afterLine.trim().slice(0, 200), blob: cp.execFileSync('git', ['-C', cfg.repo, 'rev-parse', `${cfg.rev}:${file}`]).toString().trim() });
}
process.stdout.write(JSON.stringify(res));
