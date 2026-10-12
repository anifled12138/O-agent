import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';

const moduleURL = new URL('../app/executionRouting.ts', import.meta.url).href;
function evaluate(code) {
  return JSON.parse(execFileSync(process.execPath, ['--experimental-strip-types', '--input-type=module', '-e', `import { conversationTaskBarrier, requireExecutionNode } from ${JSON.stringify(moduleURL)}; ${code}`], { encoding: 'utf8' }));
}

test('reload reconstructs local handoff barriers from durable tasks even after transcript archival or continuation import', () => {
  const result = evaluate(`
    const tasks = JSON.parse(JSON.stringify([
      { id: 'unrelated', status: 'running', payload: { sourceConversationId: 'other' } },
      { id: 'local', status: 'reported_succeeded', payload: { sourceConversationId: 'source' }, continuedConversation: { id: 'continued' } },
      { id: 'continued-task', status: 'running', payload: { conversationId: 'continued' } }
    ]));
    console.log(JSON.stringify([conversationTaskBarrier(tasks, 'source')?.id, conversationTaskBarrier(tasks, 'continued')?.id, conversationTaskBarrier(tasks, 'fresh')?.id ?? null]));
  `);
  assert.deepEqual(result, ['local', 'continued-task', null]);
});

test('running, uploading, uncertain, and unknown tasks prevent downstream dispatch; terminal cloud work can continue', () => {
  const result = evaluate(`
    const states = ['queued', 'leased', 'accepted', 'running', 'needs_reconciliation', 'future_status', 'completed', 'incomplete', 'cancelled'];
    const dispatches = [];
    for (const status of states) {
      const persisted = [{ id: status, status, progressPhase: 'uploading_artifacts', payload: { conversationId: 'source' } }];
      if (!conversationTaskBarrier(persisted, 'source')) dispatches.push(status);
    }
    console.log(JSON.stringify(dispatches));
  `);
  assert.deepEqual(result, ['completed', 'incomplete', 'cancelled']);
});

test('offline, revoked, full, or missing selected computers never fall back to another online node', () => {
  const result = evaluate(`
    const other = { id: 'other', connectivity: 'connected', resources: { maxConcurrentTasks: 2 } };
    const invalid = [null, { id: 'selected', connectivity: 'disconnected' }, { id: 'selected', connectivity: 'connected', revokedAt: 'now' }, { id: 'selected', connectivity: 'connected', resources: { maxConcurrentTasks: 0 } }];
    const dispatched = []; const errors = [];
    for (const selected of invalid) {
      try { dispatched.push(requireExecutionNode(selected ? [selected, other] : [other], 'selected').id); }
      catch (error) { errors.push(error.message); }
    }
    console.log(JSON.stringify({ dispatched, errors, valid: requireExecutionNode([other], 'other').id }));
  `);
  assert.deepEqual(result.dispatched, []);
  assert.equal(result.errors.length, 4);
  assert.equal(result.valid, 'other');
});
