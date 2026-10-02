import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';

const clientURL = new URL('../app/api.ts', import.meta.url).href;

test('browser artifact upload resumes verified chunks, retries transient failures, and reads back durable metadata', () => {
  const code = `
    import { createHash } from 'node:crypto';
    const source = Buffer.from('abcdefghij');
    const sha = (bytes) => createHash('sha256').update(bytes).digest('hex');
    const firstDigest = sha(source.subarray(0, 4));
    const calls = [];
    let secondChunkAttempts = 0;
    globalThis.window = { location: { origin: 'https://o.example' } };
    globalThis.fetch = async (url, options = {}) => {
      calls.push({ url: String(url), method: options.method ?? 'GET' });
      const path = new URL(String(url)).pathname;
      if (path === '/api/v1/artifacts/uploads') return Response.json({ upload: { id: 'upload_a', fileName: 'input.bin', expectedSize: source.length, chunkSize: 4, chunkCount: 3, status: 'uploading' }, chunkSize: 4 });
      if (path.endsWith('/uploads/upload_a')) return Response.json({ chunks: [{ index: 0, sha256: firstDigest, byteSize: 4 }], receivedChunkCount: 1 });
      if (path.endsWith('/chunks/1') && secondChunkAttempts++ === 0) return new Response('temporarily unavailable', { status: 503 });
      if (path.endsWith('/chunks/1') || path.endsWith('/chunks/2')) {
        const index = Number(path.split('/').pop());
        const bytes = Buffer.from(await options.body.arrayBuffer());
        return Response.json({ index, sha256: sha(bytes), byteSize: bytes.length });
      }
      if (path.endsWith('/uploads/upload_a/finalize')) return Response.json({ id: 'artifact_a', sha256: sha(source), byteSize: source.length, fileName: 'input.bin', mediaType: 'application/octet-stream', uploadId: 'upload_a', createdAt: '2026-10-02T00:00:00Z' });
      throw new Error('unexpected request ' + path);
    };
    const api = await import(${JSON.stringify(clientURL)});
    const progress = [];
    const artifact = await api.uploadArtifactFile(new File([source], 'input.bin'), 'retry-key', { onProgress: item => progress.push(item.uploadedBytes) });
    process.stdout.write(JSON.stringify({ artifact, progress, calls }));
  `;
  const { artifact, progress, calls } = JSON.parse(execFileSync(process.execPath, ['--experimental-strip-types', '--no-warnings', '--input-type=module', '-e', code], { encoding: 'utf8' }));
  assert.equal(artifact.id, 'artifact_a');
  assert.deepEqual(progress, [4, 8, 10]);
  assert.equal(calls.filter((call) => call.url.endsWith('/chunks/0')).length, 0, 'the verified saved chunk must be resumed');
  assert.equal(calls.filter((call) => call.url.endsWith('/chunks/1')).length, 2, 'the transient chunk failure must retry idempotently');
  assert.equal(calls.filter((call) => call.url.endsWith('/chunks/2')).length, 1);
});
