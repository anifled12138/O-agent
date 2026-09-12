'use client';

import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { request } from './api';

type Provider = { id: string; name: string; model: string };

type AgentSpec = { strategy: 'react.v1' | 'plan-react.v1'; systemPrompt: string; plannerPrompt?: string; maxSteps: number };
type Generation = { id: string; number: number; scope: string; status: string; definitionDigest: string; definition: { name: string; description: string; parentDigest?: string; spec: AgentSpec }; createdAt: string };
type Challenge = { id: string; title: string; objective: string; failureEvidence?: string; successCriteria: string; baselineGenerationId: string; status: string; updatedAt: string };
type EvalSummary = { trials: number; successes: number; successRate: number; averageTokens: number; averageDurationMillis: number };
type EvalReport = { baseline: EvalSummary; candidate: EvalSummary; frontierWins: number; regressions: number; recommendation: string; recommendationCause: string };
type Experiment = { id: string; challengeId: string; baselineGenerationId: string; candidateGenerationId: string; providerId: string; status: string; report?: EvalReport; lastError?: string; updatedAt: string };

export default function EvolutionCenter({ providers, onClose }: { providers: Provider[]; onClose: () => void }) {
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

  async function bootstrap() {
    if (!selectedChallenge || !providerId) return;
    setBusy('bootstrap'); setError('');
    try { await request(`/evolution/challenges/${selectedChallenge.id}/bootstrap`, { method: 'POST', body: JSON.stringify({ providerId, count: 2 }) }); await refresh(); }
    catch (reason) { setError(reason instanceof Error ? reason.message : '候选方案生成失败'); }
    finally { setBusy(''); }
  }

  async function evaluate() {
    if (!selectedChallenge || !selectedCandidate || !providerId || !prompt.trim()) return;
    setBusy('evaluate'); setError('');
    try {
      await request('/evolution/experiments', { method: 'POST', body: JSON.stringify({ challengeId: selectedChallenge.id, baselineGenerationId: selectedChallenge.baselineGenerationId, candidateGenerationId: selectedCandidate.id, providerId, repetitions: 1, cases: [{ id: 'frontier-001', name: '能力边界验收', prompt, evaluator: expected.trim() ? 'contains' : 'nonempty', expected }] }) });
      await refresh();
    } catch (reason) { setError(reason instanceof Error ? reason.message : '无法启动评测'); }
    finally { setBusy(''); }
  }

  async function promote(experiment: Experiment) {
    setBusy(`promote:${experiment.id}`); setError('');
    try { await request(`/evolution/generations/${experiment.candidateGenerationId}/promote`, { method: 'POST', body: JSON.stringify({ experimentId: experiment.id }) }); await refresh(); }
    catch (reason) { setError(reason instanceof Error ? reason.message : '无法晋升候选方案'); }
    finally { setBusy(''); }
  }

  return <div className="modal-backdrop evolution-backdrop"><section className="evolution-modal">
    <header className="evolution-header"><div><span className="eyebrow">受控自举</span><h2>演化实验室</h2><p>将已测量的能力缺口转化为候选 Agent。没有成对评测证据和你的确认，任何方案都不会成为稳定版本。</p></div><button onClick={onClose}>×</button></header>
    {error && <div className="evolution-error">{error}</div>}
    <div className="evolution-grid">
      <aside className="evolution-rail">
        <form className="evolution-form" onSubmit={createChallenge}><span className="eyebrow">01 / 定义能力边界</span><label>标题<input name="title" required placeholder="长程任务容易偏离目标" /></label><label>目标<textarea name="objective" required placeholder="希望提升哪项能力？" /></label><label>观察到的失败<textarea name="failureEvidence" placeholder="填写具体轨迹或行为" /></label><label>通过条件<textarea name="successCriteria" required placeholder="可观察、可验证的成功条件" /></label><button disabled={!!busy}>{busy === 'challenge' ? '正在保存…' : '创建能力边界'}</button></form>
        <div className="challenge-list"><p>能力边界</p>{challenges.map((item) => <button key={item.id} className={item.id === selectedChallengeId ? 'active' : ''} onClick={() => { setSelectedChallengeId(item.id); setSelectedCandidateId(''); }}><i className={`status-${item.status}`}/><span><b>{item.title}</b><small>{statusLabel(item.status)}</small></span></button>)}{!challenges.length && <small>还没有已测量的能力边界。</small>}</div>
      </aside>
      <main className="evolution-stage">
        {!selectedChallenge ? <div className="evolution-empty"><b>定义第一个能力边界</b><span>先记录真实限制，再修改 Agent Loop。</span></div> : <>
          <section className="frontier-summary"><div><span className="eyebrow">当前能力边界</span><h3>{selectedChallenge.title}</h3><p>{selectedChallenge.objective}</p></div><span className={`challenge-state status-${selectedChallenge.status}`}>{statusLabel(selectedChallenge.status)}</span></section>
          <section className="evolution-flow"><div className="done"><span>1</span><b>界定</b><small>测量缺口</small></div><div className={candidates.length ? 'done' : ''}><span>2</span><b>生成</b><small>不可变候选</small></div><div className={relevantExperiments.length ? 'done' : ''}><span>3</span><b>评测</b><small>成对 A/B</small></div><div className={baseline?.status === 'superseded' ? 'done' : ''}><span>4</span><b>晋升</b><small>用户确认</small></div></section>
          <section className="candidate-workbench"><div className="workbench-title"><div><span className="eyebrow">02 / 候选方案</span><h4>{baseline ? `基线 G${baseline.number} · ${baseline.definition.spec.strategy}` : '正在加载基线'}</h4></div><div className="bootstrap-actions"><select value={providerId} onChange={(event) => setProviderId(event.target.value)}><option value="">选择模型服务</option>{providers.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.model}</option>)}</select><button onClick={bootstrap} disabled={!!busy || !providerId}>{busy === 'bootstrap' ? '正在生成…' : '让 Agent 生成'}</button></div></div>
            <div className="candidate-cards">{candidates.map((item) => <button key={item.id} className={item.id === selectedCandidate?.id ? 'active' : ''} onClick={() => setSelectedCandidateId(item.id)}><span>G{item.number}</span><div><b>{item.definition.name}</b><small>{item.definition.description}</small></div><em>{item.definition.spec.strategy}<br/>{item.definition.spec.maxSteps} 步</em></button>)}{!candidates.length && <div className="candidate-empty">模型可以对提示词、Loop 策略或步数预算提出受控修改；生成后的方案默认不启用。</div>}</div>
          </section>
          {selectedCandidate && <section className="eval-workbench"><div><span className="eyebrow">03 / 成对评测</span><h4>基线 G{baseline?.number} 对比候选 G{selectedCandidate.number}</h4></div><div className="eval-inputs"><label>测试提示词<textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} /></label><label>期望包含的文本<input value={expected} onChange={(event) => setExpected(event.target.value)} placeholder="留空表示只要求非空回答" /></label><button onClick={evaluate} disabled={!!busy || !providerId}>{busy === 'evaluate' ? '正在启动…' : '运行 A/B'}</button></div></section>}
          <section className="experiment-list"><span className="eyebrow">证据记录</span>{relevantExperiments.map((item) => <article key={item.id}><div className="experiment-head"><b>{item.status === 'completed' ? recommendationLabel(item.report?.recommendation) : statusLabel(item.status)}</b><span>{item.id.slice(-8)}</span></div>{item.report ? <><div className="score-grid"><div><small>基线</small><strong>{Math.round(item.report.baseline.successRate * 100)}%</strong><em>{Math.round(item.report.baseline.averageTokens)} token</em></div><div><small>候选</small><strong>{Math.round(item.report.candidate.successRate * 100)}%</strong><em>{Math.round(item.report.candidate.averageTokens)} token</em></div><div><small>成对差异</small><strong>+{item.report.frontierWins} / -{item.report.regressions}</strong><em>胜出 / 回退</em></div></div><p>{item.report.recommendationCause}</p>{item.report.recommendation === 'promote' && <button className="promote-button" disabled={!!busy} onClick={() => promote(item)}>{busy === `promote:${item.id}` ? '正在晋升…' : '将候选方案晋升为稳定版本'}</button>}</> : <div className="experiment-running"><i/><span>{item.lastError || '正在受限评测环境中运行测试…'}</span></div>}</article>)}{!relevantExperiments.length && <div className="ledger-empty">还没有评测证据。</div>}</section>
        </>}
      </main>
    </div>
  </section></div>;
}

function statusLabel(value: string) {
  return ({ queued: '排队中', running: '运行中', completed: '已完成', failed: '失败', open: '待处理', evaluating: '评测中', candidate_selected: '已选候选', solved: '已解决', stable: '稳定', candidate: '候选', superseded: '已替代' } as Record<string, string>)[value] ?? value;
}

function recommendationLabel(value?: string) {
  return ({ promote: '建议晋升', reject: '建议拒绝', inconclusive: '证据不足', keep_baseline: '保留基线' } as Record<string, string>)[value ?? ''] ?? '评测完成';
}
