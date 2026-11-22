// ESM load hook of bgraph's Node collector (docs/dynamic-layer.md §8.2): logs the file: URL of every module Node's ESM loader loads.
// The Go side keeps the ones inside the repository.
import fs from 'node:fs';

let fd = -1;
let file = '';
let root = '';
const seen = new Set();

export function initialize(data) {
  file = data.file;
  root = String(data.root).toLowerCase().replaceAll('\\', '/');
}

export async function load(url, context, next) {
  if (url.startsWith('file:') && !seen.has(url)) {
    seen.add(url);
    const u = decodeURIComponent(url).toLowerCase();
    if (u.includes(root) && !u.includes('/node_modules/')) {
      try {
        if (fd < 0) fd = fs.openSync(file, 'a');
        fs.writeSync(fd, JSON.stringify({ ev: 'load', url }) + '\n');
      } catch {}
    }
  }
  return next(url, context);
}
