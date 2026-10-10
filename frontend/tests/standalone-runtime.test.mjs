import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rename, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { pathToFileURL } from 'node:url';
import { prepareStandaloneRuntime } from '../scripts/prepare-standalone.mjs';

async function fixture(t) {
  const root = await mkdtemp(path.join(os.tmpdir(), 'o-standalone-test-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  await writeFile(path.join(root, 'package.json'), '{}');
  await mkdir(path.join(root, 'dist', 'standalone'), { recursive: true });
  await writeFile(path.join(root, 'dist', 'standalone', 'server.js'), '');
  return root;
}
async function packageAt(root, name, source, dependencies = {}) {
  const directory = path.join(root, 'node_modules', name);
  await mkdir(directory, { recursive: true });
  await writeFile(path.join(directory, 'package.json'), JSON.stringify({ name, version: '1.0.0', type: 'module', main: 'index.js', dependencies }));
  await writeFile(path.join(directory, 'index.js'), source);
}

test('standalone framework peers execute after source node_modules is removed', async t => {
  const root = await fixture(t);
  await packageAt(root, 'react', 'export const value = "packaged-react";');
  await packageAt(root, 'scheduler', 'export const value = "scheduler";');
  await packageAt(root, 'react-dom', 'import { value as react } from "react"; import { value as scheduler } from "scheduler"; export const value = react + ":" + scheduler;', { scheduler: '*' });
  await packageAt(root, 'react-server-dom-webpack', 'export { value } from "react-dom";');
  const manifest = prepareStandaloneRuntime(root);
  assert.equal(manifest.scheduler, '1.0.0');
  await rename(path.join(root, 'node_modules'), path.join(root, 'source-unavailable'));
  const target = path.join(root, 'dist', 'standalone', 'node_modules', 'react-server-dom-webpack', 'index.js');
  assert.equal((await import(pathToFileURL(target).href)).value, 'packaged-react:scheduler');
  assert.deepEqual(JSON.parse(await readFile(path.join(root, 'dist', 'standalone', 'runtime-dependencies.json'), 'utf8')), manifest);
});

test('missing required standalone peer fails packaging instead of emitting success', async t => {
  const root = await fixture(t);
  assert.throws(() => prepareStandaloneRuntime(root), /required standalone dependency react/);
  await assert.rejects(readFile(path.join(root, 'dist', 'standalone', 'runtime-dependencies.json')), { code: 'ENOENT' });
});
