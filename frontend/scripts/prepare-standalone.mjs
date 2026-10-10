import { cpSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

// Vinext's externals manifest traces application bundles, but its standalone
// production server also imports these framework peers directly. Preserve the
// installed versions and their runtime dependencies instead of relying on a
// parent checkout's node_modules at deployment time.
export function prepareStandaloneRuntime(root) {
  const standalone = path.join(root, 'dist', 'standalone');
  if (!existsSync(path.join(standalone, 'server.js'))) throw new Error('Build standalone/server.js before packaging runtime peers');
  const resolver = createRequire(path.join(root, 'package.json'));
  const targetModules = path.join(standalone, 'node_modules');
  const copied = new Map();
  function copy(name, from, optional = false) {
    let packageFile;
    try { packageFile = from.resolve(`${name}/package.json`); }
    catch (error) {
      // Some exports maps hide package.json; inspect Node's package lookup paths.
      packageFile = (from.resolve.paths(name) ?? []).map(base => path.join(base, name, 'package.json')).find(existsSync);
      if (!packageFile) {
        if (optional) return;
        throw new Error(`Cannot package required standalone dependency ${name}`, { cause: error });
      }
    }
    const metadata = JSON.parse(readFileSync(packageFile, 'utf8'));
    const previous = copied.get(name);
    if (previous) {
      if (previous !== metadata.version) throw new Error(`Conflicting runtime versions for ${name}: ${previous} and ${metadata.version}`);
      return;
    }
    const target = path.join(targetModules, name);
    const existing = path.join(target, 'package.json');
    if (existsSync(existing) && JSON.parse(readFileSync(existing, 'utf8')).version !== metadata.version) {
      throw new Error(`Standalone ${name} differs from the installed runtime version`);
    }
    mkdirSync(path.dirname(target), { recursive: true });
    const source = path.dirname(packageFile);
    cpSync(source, target, {
      recursive: true, dereference: true,
      filter: candidate => !path.relative(source, candidate).split(path.sep).includes('node_modules'),
    });
    copied.set(name, metadata.version);
    const dependencyResolver = createRequire(packageFile);
    const optionalDependencies = metadata.optionalDependencies ?? {};
    for (const dependency of Object.keys({ ...metadata.dependencies, ...optionalDependencies })) {
      copy(dependency, dependencyResolver, Object.hasOwn(optionalDependencies, dependency));
    }
  }
  for (const peer of ['react', 'react-dom', 'react-server-dom-webpack']) copy(peer, resolver);
  const manifest = Object.fromEntries(copied);
  writeFileSync(path.join(standalone, 'runtime-dependencies.json'), JSON.stringify(manifest, null, 2) + '\n');
  return manifest;
}
if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
  console.log('Packaged standalone framework runtime peers:', prepareStandaloneRuntime(root));
}
