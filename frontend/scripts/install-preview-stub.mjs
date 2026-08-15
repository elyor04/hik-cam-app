#!/usr/bin/env node
/**
 * Copies frontend/preview-stub/ into frontend/wailsjs/ so `npm run dev` can render the UI on a
 * machine that cannot build the Go backend.
 *
 * frontend/wailsjs/ is generated output and stays gitignored -- `wails dev`/`wails build` own it and
 * overwrite it wholesale. The stub that stands in for it on macOS is hand-written and worth
 * keeping, so its source of truth lives in preview-stub/ (tracked) and is *installed* into place
 * rather than being edited in the ignored directory. Editing wailsjs/ directly is how the previous
 * copy came to exist only on one machine.
 *
 * Refuses to overwrite real generated bindings: if wailsjs/ holds Wails output rather than a
 * previous install of this stub, installing over it would replace working bindings with demo data
 * and the failure would look like the backend returning nonsense. Pass --force to do it anyway
 * (after which `wails dev` will regenerate the real ones).
 */
import {readFileSync, writeFileSync, mkdirSync, existsSync, readdirSync, statSync, rmSync} from 'node:fs';
import {join, dirname, resolve, relative} from 'node:path';
import {fileURLToPath} from 'node:url';

const frontendDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const srcDir = join(frontendDir, 'preview-stub');
const dstDir = join(frontendDir, 'wailsjs');
const force = process.argv.includes('--force');

const STUB_MARKER = 'FRONTEND-ONLY PREVIEW STUB';

if (!existsSync(srcDir)) {
  console.error(`install-preview-stub: ${relative(frontendDir, srcDir)} not found.`);
  process.exit(1);
}

const existingEntry = join(dstDir, 'go', 'main', 'App.ts');
if (!force && existsSync(existingEntry)) {
  const current = readFileSync(existingEntry, 'utf8');
  if (!current.includes(STUB_MARKER)) {
    console.error('install-preview-stub: wailsjs/ holds real generated bindings, refusing to overwrite them.');
    console.error('Delete frontend/wailsjs/ first, or pass --force, if you really want the preview stub here.');
    process.exit(1);
  }
}

function copyTree(from, to) {
  mkdirSync(to, {recursive: true});
  for (const entry of readdirSync(from)) {
    const src = join(from, entry);
    const dst = join(to, entry);
    if (statSync(src).isDirectory()) {
      copyTree(src, dst);
    } else {
      writeFileSync(dst, readFileSync(src));
    }
  }
}

// Clear first so a file deleted from the source cannot survive in the installed copy.
if (existsSync(dstDir)) rmSync(dstDir, {recursive: true, force: true});
copyTree(srcDir, dstDir);

console.log('install-preview-stub: installed frontend/preview-stub/ -> frontend/wailsjs/');
console.log('Run `wails dev`/`wails build` on a machine with the SDK vendored to replace it with real bindings.');
