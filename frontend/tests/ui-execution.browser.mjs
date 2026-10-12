import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

// Run against a local renderer preview. APIs are intercepted; no real task is executed.
const playwrightModule = process.env.O_PLAYWRIGHT_MODULE;
const { chromium } = await import(playwrightModule ? pathToFileURL(playwrightModule).href : 'playwright');
const browser = await chromium.launch({ headless: true, ...(process.env.O_CHROME_PATH ? { executablePath: process.env.O_CHROME_PATH } : {}) });
const output = process.env.O_UI_SCREENSHOTS || '.tmp-ui-screenshots';
await mkdir(output, { recursive: true });
const provider = { id: 'p1', name: 'Default provider', model: 'claude-opus', hasApiKey: true, contextWindow: 100000 };
const source = { id: 'source', title: '检查项目架构', providerId: 'p1', projectId: '', permissionProfile: 'workspace_autonomous', executionPaused: false, updatedAt: new Date().toISOString(), messages: [
  { id: 'u1', role: 'user', content: '请检查这个项目的架构。' },
  { id: 'a1', role: 'assistant', content: '会话和产物需要在执行前同步。\n\n手机上应为回答留出左右边距，让长段落阅读更舒服。我们保留清晰的层级，并将输入框的设置收进图标菜单。\n\n```js\nconst workspace = await synchronizeConversationAndArtifacts();\n```' }
] };
let tasks = [];
let nodes = [{ id: 'node1', name: '书房电脑', platform: 'windows', connectivity: 'connected', resources: { maxConcurrentTasks: 1 }, catalog: { models: [{ id: 'local1', name: 'Local provider', model: 'local-model' }] } }];
let taskReadFails = false;
let continuationFails = false;
let continuation;
const mutations = [];
const pageErrors = [];
const context = await browser.newContext({ viewport: { width: 375, height: 812 }, isMobile: true, hasTouch: true, reducedMotion: 'reduce' });
const page = await context.newPage();
page.on('pageerror', (error) => pageErrors.push(error.message));
await page.route('**/api/**', async (route) => {
  const request = route.request();
  const path = new URL(request.url()).pathname.replace(/^\/api\/v[12]/, '');
  if (request.method() !== 'GET') mutations.push({ path, body: request.postDataJSON() });
  let body = [];
  if (path === '/auth/status') body = { authenticationRequired: false };
  else if (path === '/providers') body = [provider];
  else if (path === '/plugins') body = [{ id: 'core:conversation_title', type: 'core', status: 'disabled' }];
  else if (path === '/conversations') body = continuation ? [source, continuation] : [source];
  else if (path === '/conversations/source') body = source;
  else if (continuation && path === '/conversations/continued') body = continuation;
  else if (path === '/nodes') body = nodes;
  else if (path === '/tasks') {
    if (taskReadFails) return route.fulfill({ status: 503, json: { error: 'task state unavailable' } });
    body = tasks;
  } else if (path === '/conversations/source/tasks/nodes') {
    const input = request.postDataJSON();
    const task = { id: 'success-task', nodeId: input.targetNodeId, status: 'queued', payload: { sourceConversationId: 'source', targetNodeId: input.targetNodeId, localProviderId: input.localProviderId, content: input.content } };
    tasks = [task]; body = { task, created: true };
  } else if (path === '/tasks/success-task') body = tasks.find((task) => task.id === 'success-task');
  else if (path === '/tasks/local-task') body = tasks.find((task) => task.id === 'local-task');
  else if (path === '/tasks/continued-task') body = tasks.find((task) => task.id === 'continued-task');
  else if (path === '/tasks/local-task/continuation-preview') body = { taskId: 'local-task', effects: [], requiresAcknowledgement: false };
  else if (path === '/tasks/local-task/continue-in-cloud') {
    if (continuationFails) return route.fulfill({ status: 503, json: { error: 'artifact verification failed' } });
    continuation = { ...source, id: 'continued', title: '检查项目架构 · 云端续接', parentConversationId: 'source', messages: [...source.messages, { id: 'a2', role: 'assistant', content: '本地完成的工作与产物已同步到这里。' }] };
    const task = { id: 'continued-task', parentTaskId: 'local-task', nodeId: 'node_cloud_test', status: 'queued', payload: { conversationId: 'continued' } };
    tasks = [{ ...tasks[0], continuedConversation: continuation, continuationTaskId: task.id, continuationTaskStatus: task.status }, task];
    body = { conversation: continuation, project: null, task };
  }
  return route.fulfill({ json: body });
});
try {
  await page.goto(process.env.O_UI_TEST_URL || 'http://127.0.0.1:5187');
  await page.getByRole('textbox', { name: '输入消息内容' }).waitFor();
  const toolbar = await page.locator('.composer-row').boundingBox();
  const controls = [];
  for (const name of ['附件与插件', '会话权限模式：工作区自动', '选择模型，当前 claude-opus', '发送']) controls.push(await page.getByRole('button', { name, exact: true }).boundingBox());
  assert(controls.every((box) => box && box.width >= 44 && box.height >= 44 && Math.abs(box.y - controls[0].y) < 2));
  assert(toolbar.height < 64, 'mobile toolbar must fit on one row');
  assert(await page.locator('.composer-attach-button').isHidden());
  assert(await page.locator('.composer-plugin-trigger').isHidden());
  await page.getByRole('button', { name: '附件与插件' }).click();
  assert(await page.getByRole('button', { name: '插件与能力' }).isVisible());
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '选择模型，当前 claude-opus' }).click();
  const modelMenu = await page.getByRole('dialog', { name: '选择模型' }).boundingBox();
  assert(modelMenu.x >= 12 && modelMenu.x + modelMenu.width <= 363);
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '展开侧边栏' }).click();
  await page.locator('.sidebar-convo-btn').filter({ hasText: '检查项目架构' }).click();
  const answer = await page.locator('.message.assistant .message-body').boundingBox();
  assert(answer.x >= 20 && answer.x + answer.width <= 355, 'answer needs comfortable gutters');
  async function assertHeaderAlignment(width) {
    const boxes = await page.locator('.header-title-interactive-block, .project-header-badge, .header-actions > .icon-button, .header-actions > .status-pill').evaluateAll((elements) => elements.map((element) => {
      const box = element.getBoundingClientRect();
      return { height: box.height, center: box.y + box.height / 2 };
    }));
    assert.equal(boxes.length, 4);
    const height = width <= 720 ? 44 : 32;
    assert(boxes.every((box) => Math.abs(box.height - height) < .5), `header control heights at ${width}: ${JSON.stringify(boxes)}`);
    assert(boxes.every((box) => Math.abs(box.center - boxes[0].center) < .5), `header centers at ${width}: ${JSON.stringify(boxes)}`);
  }
  await assertHeaderAlignment(375);
  await page.screenshot({ path: `${output}/mobile-375.png` });
  for (const width of [320, 390, 768, 1280]) {
    await page.setViewportSize({ width, height: 812 });
    await assertHeaderAlignment(width);
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `no page overflow at ${width}`);
    if (width <= 390) { const row = await page.locator('.composer-row').boundingBox(); assert(row.height < 64); }
  }
  await page.getByRole('button', { name: '展开侧边栏' }).click();
  await page.getByRole('button', { name: '执行位置 当前服务节点' }).click();
  await page.screenshot({ path: `${output}/desktop-1280.png` });
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '收起侧边栏' }).click();
  await page.setViewportSize({ width: 812, height: 375 });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
  await page.setViewportSize({ width: 375, height: 812 });
  await page.addStyleTag({ content: '.message.assistant .markdown-view { font-size: 24px !important; }' });
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
  await page.getByRole('button', { name: '展开侧边栏' }).click();
  await page.getByRole('button', { name: '执行位置 当前服务节点' }).click();
  await page.getByRole('button', { name: '书房电脑 windows' }).click();
  await page.getByRole('button', { name: '关闭侧边栏' }).click();
  assert(await page.getByRole('button', { name: '选择模型，当前 local-model' }).isVisible(), 'device model belongs in unified model menu');
  // Revoke the chosen node between selection and dispatch; never send to another machine.
  nodes = [{ ...nodes[0], connectivity: 'disconnected' }, { ...nodes[0], id: 'node2', name: '另一台电脑' }];
  await page.getByRole('textbox', { name: '输入消息内容' }).fill('继续执行');
  await page.getByRole('button', { name: '发送', exact: true }).click();
  await page.getByText('所选电脑已离线或已撤销，请重新选择执行设备。').waitFor();
  assert.equal(mutations.length, 0);
  assert.equal(await page.getByRole('textbox', { name: '输入消息内容' }).inputValue(), '继续执行');
  nodes = [{ ...nodes[0], connectivity: 'connected' }];
  await page.getByRole('button', { name: '发送', exact: true }).click();
  await page.getByRole('button', { name: '停止会话', exact: true }).waitFor();
  const dispatch = mutations.find((item) => item.path === '/conversations/source/tasks/nodes');
  assert.equal(dispatch.body.targetNodeId, 'node1');
  assert.equal(dispatch.body.localProviderId, 'local1');
  assert.equal(dispatch.body.content, '继续执行');
  assert.equal(tasks[0].payload.sourceConversationId, 'source');
  assert.equal(await page.getByRole('textbox', { name: '输入消息内容' }).inputValue(), '');
  const priorMutationCount = mutations.length;
  // Reconstruct an unsynchronized local result after reload.
  tasks = [{ id: 'local-task', nodeId: 'node1', status: 'reported_succeeded', payload: { sourceConversationId: 'source' }, result: { sourceConversationId: 'source', agentStatus: 'completed', transcriptArtifact: { id: 'transcript', fileName: 'transcript.json' } } }];
  await page.reload();
  await page.getByRole('button', { name: '展开侧边栏' }).click();
  await page.locator('.sidebar-convo-btn').filter({ hasText: '检查项目架构' }).click();
  await page.getByRole('textbox', { name: '输入消息内容' }).fill('不要遗漏本地产物');
  await page.getByRole('button', { name: '发送', exact: true }).click();
  await page.getByRole('dialog', { name: '任务与产物' }).waitFor();
  assert.equal(mutations.length, priorMutationCount, 'no task dispatched with stale source history');
  continuationFails = true;
  await page.getByRole('button', { name: '创建云端续接会话' }).click();
  await page.getByText('artifact verification failed').waitFor();
  assert(await page.getByRole('dialog', { name: '任务与产物' }).isVisible(), 'failed handoff retains review');
  assert.equal(tasks[0].status, 'reported_succeeded');
  continuationFails = false;
  await page.getByRole('button', { name: '创建云端续接会话' }).click();
  await page.getByText('本地完成的工作与产物已同步到这里。').waitFor();
  assert(await page.getByRole('button', { name: '停止会话', exact: true }).isVisible(), 'continuation task remains active until durable terminal status');
  assert.equal(mutations.filter((item) => item.path.includes('/turns') || item.path.includes('/tasks/nodes') || item.path.includes('/tasks/cloud')).length, 1);
  assert.deepEqual(pageErrors, []);
  // Execution status failures cannot change the destination.
  tasks = [];
  await page.reload();
  await page.getByRole('button', { name: '展开侧边栏' }).click();
  await page.getByRole('button', { name: '执行位置 当前服务节点' }).click();
  taskReadFails = true;
  await page.getByRole('button', { name: '云端任务队列', exact: true }).click();
  await page.getByRole('button', { name: '关闭侧边栏' }).click();
  await page.getByText('task state unavailable').waitFor();
  await page.getByRole('button', { name: '展开侧边栏' }).click();
  assert(await page.getByRole('button', { name: '执行位置 当前服务节点' }).isVisible());
  console.log('PASS: mobile toolbar/gutters, responsive widths, keyboard menus, device catalog, offline dispatch, pinned-node/model dispatch, restart handoff barrier, failed handoff retry, continuation observation, failed-switch rollback.');
} finally { await browser.close(); }
