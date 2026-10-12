import type { ExecutionNode, ExecutionTask } from './api';

export type ExecutionTarget = 'current' | 'cloud' | 'local';
export type RoutedExecutionTask = ExecutionTask & { payload?: { conversationId?: string; sourceConversationId?: string } };

// Reconstruct from durable task records after reload and before submission.
// Archived local results must be imported before the source can be continued.
export function conversationTaskBarrier(tasks: RoutedExecutionTask[], conversationId?: string): RoutedExecutionTask | undefined {
  if (!conversationId) return undefined;
  const matching = tasks.filter((task) => task.payload?.conversationId === conversationId || task.payload?.sourceConversationId === conversationId);
  return matching.find((task) => !['completed', 'incomplete', 'reported_succeeded', 'reported_failed', 'cancelled', 'needs_reconciliation'].includes(task.status))
    ?? matching.find((task) => task.status === 'needs_reconciliation' || (task.payload?.sourceConversationId === conversationId && task.status === 'reported_succeeded'));
}

export function requireExecutionNode(nodes: ExecutionNode[], nodeId: string): ExecutionNode {
  const node = nodes.find((item) => item.id === nodeId);
  if (!node || node.connectivity !== 'connected' || node.revokedAt) throw new Error('所选电脑已离线或已撤销，请重新选择执行设备。');
  if (node.resources?.maxConcurrentTasks === 0) throw new Error('所选电脑暂时没有可用执行容量，请稍后重试或选择其他设备。');
  return node;
}

export const executionPhaseLabels: Record<string, string> = {
  preparing: '正在同步会话与工作区', agent_and_snapshot: '正在执行与保存快照', waiting_for_safe_boundary: '等待安全暂停', safe_boundary_reached: '已安全暂停，正在归档',
  running_agent: '正在执行', handoff_requested: '等待安全暂停', local_result_durable: '正在归档结果',
  uploading_artifacts: '正在同步产物', artifacts_verified: '正在核验同步结果',
};
