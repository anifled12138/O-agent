import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { X } from 'lucide-react';
import { request } from './api';
import { useDialogA11y } from './useDialogA11y';

type Provider = { id: string; name: string; model: string };

type AgentSpec = { strategy: 'react.v1' | 'plan-react.v1'; systemPrompt: string; plannerPrompt?: string; maxSteps: number };
type Generation = { id: string; number: number; scope: string; status: string; definitionDigest: string; definition: { name: string; description: string; parentDigest?: string; spec: AgentSpec }; createdAt: string };
type Challenge = { id: string; title: string; objective: string; failureEvidence?: string; successCriteria: string; baselineGenerationId: string; status: string; updatedAt: string };
type EvalSummary = { trials: number; successes: number; successRate: number; averageTokens: number; averageDurationMillis: number };
type EvalReport = { baseline: EvalSummary; candidate: EvalSummary; frontierWins: number; regressions: number; recommendation: string; recommendationCause: string };
type Experiment = { id: string; challengeId: string; baselineGenerationId: string; candidateGenerationId: string; providerId: string; status: string; report?: EvalReport; lastError?: string; updatedAt: string };

export default function EvolutionCenter({ providers, onClose }: { providers: Provider[]; onClose: () => void }) {
  const dialogRef = useDialogA11y(onClose);
  const [generations, setGenerations] = useState<Generation[]>([]);
  const [challenges, setChallenges] = useState<Challenge[]>([]);
  const [experiments, setExperiments] = useState<Experiment[]>([]);
  const [selectedChallengeId, setSelectedChallengeId] = useState('');
  const [selectedCandidateId, setSelectedCandidateId] = useState('');
  const [providerId, setProviderId] = useState(providers[0]?.id ?? '');
  const [prompt, setPrompt] = useState('完成这个边界任务，并返回要求的结果。');
  const [expected, setExpected] = useState('完成');
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');

  const refresh = useCallback(async () => {
    const [nextGenerations, nextChallenges, nextExperiments] = await Promise.all([
      request<Generation[]>('/evolution/generations'),
      request<Challenge[]>('/evolution/challenges'),
      request<Experiment[]>('/evolution/experiments'),
    ]);
    setGenerations(nextGenerations); setChallenges(nextChallenges); setExperiments(nextExperiments);
    setSelectedChallengeId((current) => current || nextChallenges[0]?.id || '');
  }, []);

  useEffect(() => {
    const timer = window.setTimeout(() => void refresh().catch((reason) => setError(reason instanceof Error ? reason.message : '无法加载演化状态')), 0);
    return () => window.clearTimeout(timer);
  }, [refresh]);
  useEffect(() => {
    if (!experiments.some((item) => item.status === 'queued' || item.status === 'running')) return;
    const timer = window.setInterval(() => void refresh(), 1200);
    return () => window.clearInterval(timer);
  }, [experiments, refresh]);

  const selectedChallenge = challenges.find((item) => item.id === selectedChallengeId);
  const baseline = generations.find((item) => item.id === selectedChallenge?.baselineGenerationId);
  const candidates = useMemo(() => generations.filter((item) => item.status === 'candidate' && (!baseline || item.definition.parentDigest === baseline.definitionDigest)), [generations, baseline]);
  const selectedCandidate = candidates.find((item) => item.id === selectedCandidateId) ?? candidates[0];
  const relevantExperiments = experiments.filter((item) => !selectedChallenge || item.challengeId === selectedChallenge.id);

  async function createChallenge(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); setBusy('challenge'); setError('');
    const form = new FormData(event.currentTarget);
    try {
      const challenge = await request<Challenge>('/evolution/challenges', { method: 'POST', body: JSON.stringify({ title: form.get('title'), objective: form.get('objective'), failureEvidence: form.get('failureEvidence'), successCriteria: form.get('successCriteria') }) });
      await refresh(); setSelectedChallengeId(challenge.id); event.currentTarget.reset();
    } catch (reason) { setError(reason instanceof Error ? reason.message : '无法创建能力边界'); }
    finally { setBusy(''); }
  }

  async function bootstrapCandidate() {
    if (!selectedChallenge) return;
    setBusy('bootstrap'); setError('');
    try {
      const created = await request<Generation>(`/evolution/challenges/${selectedChallenge.id}/bootstrap`, { method: 'POST', body: JSON.stringify({ providerId }) });
      await refresh(); setSelectedCandidateId(created.id);
    } catch (reason) { setError(reason instanceof Error ? reason.message : '候选生成失败'); }
    finally { setBusy(''); }
  }

  async function evaluateCandidate() {
    if (!selectedChallenge || !selectedCandidate) return;
    setBusy('eval'); setError('');
    try {
      await request<Experiment>('/evolution/experiments', { method: 'POST', body: JSON.stringify({ challengeId: selectedChallenge.id, candidateGenerationId: selectedCandidate.id, providerId, prompt, expected }) });
      await refresh();
    } catch (reason) { setError(reason instanceof Error ? reason.message : '启动评测失败'); }
    finally { setBusy(''); }
  }

  async function promoteCandidate() {
    if (!selectedCandidate) return;
    setBusy('promote'); setError('');
    try {
      await request<Generation>(`/evolution/generations/${selectedCandidate.id}/promote`, { method: 'POST' });
      await refresh();
    } catch (reason) { setError(reason instanceof Error ? reason.message : '晋升失败'); }
    finally { setBusy(''); }
  }

  return (
    <div className="modal-backdrop evolution-backdrop" onClick={(e) => { if (e.target === e.currentTarget) onClose(); }}>
      <section ref={dialogRef} className="evolution-modal" role="dialog" aria-modal="true" aria-labelledby="evolution-dialog-title" tabIndex={-1}>
        <header className="evolution-header">
          <div>
            <span className="eyebrow">受控自举</span>
            <h2 id="evolution-dialog-title">演化实验室</h2>
            <p>将已测量的能力缺口转化为候选 Agent。没有成对评测证据和你的确认，任何方案都不会成为稳定版本。</p>
          </div>
          <button type="button" onClick={onClose} aria-label="关闭演化实验室" className="icon-button">
            <X size={18} strokeWidth={2} aria-hidden="true" />
          </button>
        </header>
        {error && <div className="evolution-error" role="alert">{error}</div>}
        <div className="evolution-grid">
          <aside className="evolution-rail">
            <form onSubmit={createChallenge} className="evolution-form">
              <span className="eyebrow">定义新边界</span>
              <label>挑战标题<input name="title" placeholder="复杂工具递归恢复" required /></label>
              <label>目标能力<textarea name="objective" placeholder="要求 Agent 能够定位深层错误并执行安全修复…" required /></label>
              <label>失败证据<textarea name="failureEvidence" placeholder="在 turn_xxx 中，出现未知工具错误并停止…" /></label>
              <label>成功标准<input name="successCriteria" placeholder="产生验证通过的补丁并通过构建测试" required /></label>
              <button type="submit" disabled={busy === 'challenge'}>{busy === 'challenge' ? '正在提交…' : '创建能力边界'}</button>
            </form>
            <div className="challenge-list">
              <p>能力边界 ({challenges.length})</p>
              {challenges.length === 0 ? <small>暂无挑战，请在上方创建。</small> : challenges.map((item) => (
                <button type="button" key={item.id} className={item.id === selectedChallengeId ? 'active' : ''} onClick={() => setSelectedChallengeId(item.id)}>
                  <i className={`status-${item.status}`} />
                  <span><b>{item.title}</b><small>{item.status}</small></span>
                </button>
              ))}
            </div>
          </aside>

          <div className="evolution-stage">
            {selectedChallenge ? (
              <>
                <div className="frontier-summary">
                  <div>
                    <span className="eyebrow">挑战详情</span>
                    <h3>{selectedChallenge.title}</h3>
                    <p>{selectedChallenge.objective}</p>
                  </div>
                  <span className={`challenge-state status-${selectedChallenge.status}`}>{selectedChallenge.status}</span>
                </div>

                <div className="evolution-flow">
                  <div className="done"><span>1</span><div><b>测量缺口</b><small>基准 #{baseline?.number ?? 1}</small></div></div>
                  <div className={selectedCandidate ? 'done' : ''}><span>2</span><div><b>生成候选</b><small>{selectedCandidate ? `#${selectedCandidate.number}` : '待生成'}</small></div></div>
                  <div className={relevantExperiments.some((e) => e.status === 'completed') ? 'done' : ''}><span>3</span><div><b>成对评测</b><small>{relevantExperiments.length} 轮</small></div></div>
                  <div className={selectedChallenge.status === 'solved' ? 'done' : ''}><span>4</span><div><b>人工批准</b><small>安全晋升</small></div></div>
                </div>

                <section className="candidate-workbench">
                  <div className="workbench-title">
                    <div>
                      <span className="eyebrow">候选生成</span>
                      <h4>候选 Agent 版本</h4>
                    </div>
                    <div className="bootstrap-actions">
                      <select value={providerId} onChange={(e) => setProviderId(e.target.value)} aria-label="选择模型服务">
                        {providers.map((p) => <option key={p.id} value={p.id}>{p.name} ({p.model})</option>)}
                      </select>
                      <button type="button" disabled={busy === 'bootstrap'} onClick={bootstrapCandidate}>
                        {busy === 'bootstrap' ? '正在生成…' : '基于基准生成候选'}
                      </button>
                    </div>
                  </div>
                  <div className="candidate-cards">
                    {candidates.length === 0 ? (
                      <div className="candidate-empty">尚无生成的候选版本。请点击右上角按钮由模型生成候选 Spec。</div>
                    ) : (
                      candidates.map((item) => (
                        <button type="button" key={item.id} className={item.id === selectedCandidate?.id ? 'active' : ''} onClick={() => setSelectedCandidateId(item.id)}>
                          <span>#{item.number}</span>
                          <div>
                            <b>{item.definition.name}</b>
                            <small>{item.definition.description || '无详细描述'}</small>
                          </div>
                          <em>{item.status}</em>
                        </button>
                      ))
                    )}
                  </div>
                </section>

                <section className="eval-workbench">
                  <div>
                    <span className="eyebrow">双盲评测</span>
                    <h4>运行验证实验</h4>
                    <p style={{ fontSize: '12px', color: 'var(--muted)', marginTop: '4px' }}>
                      同时向基准与候选注入相同的目标与测试输入，收集成功率、步数与 Token 消耗。
                    </p>
                  </div>
                  <div className="eval-inputs">
                    <textarea value={prompt} onChange={(e) => setPrompt(e.target.value)} placeholder="评测目标…" aria-label="评测目标" />
                    <input value={expected} onChange={(e) => setExpected(e.target.value)} placeholder="期望产出…" aria-label="期望产出" />
                    <button type="button" disabled={busy === 'eval' || !selectedCandidate} onClick={evaluateCandidate}>
                      {busy === 'eval' ? '评测中…' : '执行评测'}
                    </button>
                  </div>
                </section>

                <section className="experiment-list">
                  <span className="eyebrow">评测记录与成对证据</span>
                  {relevantExperiments.length === 0 ? (
                    <div className="ledger-empty">尚未对此挑战执行实验评测。</div>
                  ) : (
                    relevantExperiments.map((exp) => (
                      <article key={exp.id}>
                        <div className="experiment-head">
                          <span>实验 #{exp.id.slice(0, 8)} · 状态: {exp.status}</span>
                          {exp.report && <b>胜出率: {exp.report.frontierWins} / 净提升: {exp.report.frontierWins - exp.report.regressions}</b>}
                        </div>
                        {exp.report ? (
                          <div className="score-grid">
                            <div><small>基准成功率</small><strong>{Math.round(exp.report.baseline.successRate * 100)}%</strong><em>{exp.report.baseline.averageTokens} tok</em></div>
                            <div><small>候选成功率</small><strong>{Math.round(exp.report.candidate.successRate * 100)}%</strong><em>{exp.report.candidate.averageTokens} tok</em></div>
                            <div><small>建议结论</small><strong>{exp.report.recommendation === 'promote' ? '建议晋升' : '建议改进'}</strong><em>{exp.report.recommendationCause}</em></div>
                          </div>
                        ) : (
                          <div className="experiment-running"><i />评测正在运行中，已完成部分试验…</div>
                        )}
                      </article>
                    ))
                  )}
                  {selectedCandidate && (
                    <button type="button" className="promote-button" disabled={busy === 'promote'} onClick={promoteCandidate}>
                      {busy === 'promote' ? '正在应用…' : `批准并晋升候选 #${selectedCandidate.number} 为稳定版`}
                    </button>
                  )}
                </section>
              </>
            ) : (
              <div className="evolution-empty">
                <b>选择或创建一个能力边界</b>
                <span>系统将针对具体的失败案例实施闭环自举。</span>
              </div>
            )}
          </div>
        </div>
      </section>
    </div>
  );
}
