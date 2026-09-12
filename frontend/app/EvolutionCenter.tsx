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
  const [prompt, setPrompt] = useState('Complete the frontier task and return the requested result.');
  const [expected, setExpected] = useState('DONE');
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
    const timer = window.setTimeout(() => void refresh().catch((reason) => setError(reason instanceof Error ? reason.message : 'Evolution state could not be loaded')), 0);
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
    } catch (reason) { setError(reason instanceof Error ? reason.message : 'Challenge creation failed'); }
    finally { setBusy(''); }
  }

  async function bootstrap() {
    if (!selectedChallenge || !providerId) return;
    setBusy('bootstrap'); setError('');
    try { await request(`/evolution/challenges/${selectedChallenge.id}/bootstrap`, { method: 'POST', body: JSON.stringify({ providerId, count: 2 }) }); await refresh(); }
    catch (reason) { setError(reason instanceof Error ? reason.message : 'Candidate generation failed'); }
    finally { setBusy(''); }
  }

  async function evaluate() {
    if (!selectedChallenge || !selectedCandidate || !providerId || !prompt.trim()) return;
    setBusy('evaluate'); setError('');
    try {
      await request('/evolution/experiments', { method: 'POST', body: JSON.stringify({ challengeId: selectedChallenge.id, baselineGenerationId: selectedChallenge.baselineGenerationId, candidateGenerationId: selectedCandidate.id, providerId, repetitions: 1, cases: [{ id: 'frontier-001', name: 'Frontier acceptance', prompt, evaluator: expected.trim() ? 'contains' : 'nonempty', expected }] }) });
      await refresh();
    } catch (reason) { setError(reason instanceof Error ? reason.message : 'Evaluation failed to start'); }
    finally { setBusy(''); }
  }

  async function promote(experiment: Experiment) {
    setBusy(`promote:${experiment.id}`); setError('');
    try { await request(`/evolution/generations/${experiment.candidateGenerationId}/promote`, { method: 'POST', body: JSON.stringify({ experimentId: experiment.id }) }); await refresh(); }
    catch (reason) { setError(reason instanceof Error ? reason.message : 'Promotion failed'); }
    finally { setBusy(''); }
  }

  return <div className="modal-backdrop evolution-backdrop"><section className="evolution-modal">
    <header className="evolution-header"><div><span className="eyebrow">BOUNDED SELF-BOOTSTRAP</span><h2>Evolution Lab</h2><p>Turn a measured capability gap into a candidate Agent generation. Nothing becomes stable without paired evidence and your promotion.</p></div><button onClick={onClose}>×</button></header>
    {error && <div className="evolution-error">{error}</div>}
    <div className="evolution-grid">
      <aside className="evolution-rail">
        <form className="evolution-form" onSubmit={createChallenge}><span className="eyebrow">01 / DEFINE FRONTIER</span><label>TITLE<input name="title" required placeholder="Long tasks lose direction" /></label><label>OBJECTIVE<textarea name="objective" required placeholder="What ability should improve?" /></label><label>OBSERVED FAILURE<textarea name="failureEvidence" placeholder="Concrete trace or behavior" /></label><label>PASS CONDITION<textarea name="successCriteria" required placeholder="Observable success condition" /></label><button disabled={!!busy}>{busy === 'challenge' ? 'Saving…' : 'Create challenge'}</button></form>
        <div className="challenge-list"><p>CHALLENGES</p>{challenges.map((item) => <button key={item.id} className={item.id === selectedChallengeId ? 'active' : ''} onClick={() => { setSelectedChallengeId(item.id); setSelectedCandidateId(''); }}><i className={`status-${item.status}`}/><span><b>{item.title}</b><small>{item.status}</small></span></button>)}{!challenges.length && <small>No measured frontier yet.</small>}</div>
      </aside>
      <main className="evolution-stage">
        {!selectedChallenge ? <div className="evolution-empty"><b>Define the first frontier</b><span>Capture an actual limitation before changing the loop.</span></div> : <>
          <section className="frontier-summary"><div><span className="eyebrow">ACTIVE FRONTIER</span><h3>{selectedChallenge.title}</h3><p>{selectedChallenge.objective}</p></div><span className={`challenge-state status-${selectedChallenge.status}`}>{selectedChallenge.status}</span></section>
          <section className="evolution-flow"><div className="done"><span>1</span><b>Scope</b><small>measured gap</small></div><div className={candidates.length ? 'done' : ''}><span>2</span><b>Generate</b><small>immutable candidates</small></div><div className={relevantExperiments.length ? 'done' : ''}><span>3</span><b>Evaluate</b><small>paired A/B</small></div><div className={baseline?.status === 'superseded' ? 'done' : ''}><span>4</span><b>Promote</b><small>user authority</small></div></section>
          <section className="candidate-workbench"><div className="workbench-title"><div><span className="eyebrow">02 / CANDIDATE GENERATIONS</span><h4>{baseline ? `Baseline G${baseline.number} · ${baseline.definition.spec.strategy}` : 'Loading baseline'}</h4></div><div className="bootstrap-actions"><select value={providerId} onChange={(event) => setProviderId(event.target.value)}><option value="">Select model provider</option>{providers.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.model}</option>)}</select><button onClick={bootstrap} disabled={!!busy || !providerId}>{busy === 'bootstrap' ? 'Generating…' : 'Generate with Agent'}</button></div></div>
            <div className="candidate-cards">{candidates.map((item) => <button key={item.id} className={item.id === selectedCandidate?.id ? 'active' : ''} onClick={() => setSelectedCandidateId(item.id)}><span>G{item.number}</span><div><b>{item.definition.name}</b><small>{item.definition.description}</small></div><em>{item.definition.spec.strategy}<br/>{item.definition.spec.maxSteps} steps</em></button>)}{!candidates.length && <div className="candidate-empty">The model will propose bounded changes to prompts, loop strategy, or step budget. Generated definitions remain inactive.</div>}</div>
          </section>
          {selectedCandidate && <section className="eval-workbench"><div><span className="eyebrow">03 / PAIRED HARNESS</span><h4>Baseline G{baseline?.number} vs Candidate G{selectedCandidate.number}</h4></div><div className="eval-inputs"><label>TEST PROMPT<textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} /></label><label>EXPECTED SUBSTRING<input value={expected} onChange={(event) => setExpected(event.target.value)} placeholder="Empty means non-empty response" /></label><button onClick={evaluate} disabled={!!busy || !providerId}>{busy === 'evaluate' ? 'Starting…' : 'Run A/B'}</button></div></section>}
          <section className="experiment-list"><span className="eyebrow">EVIDENCE LEDGER</span>{relevantExperiments.map((item) => <article key={item.id}><div className="experiment-head"><b>{item.status === 'completed' ? item.report?.recommendation.toUpperCase() : item.status.toUpperCase()}</b><span>{item.id.slice(-8)}</span></div>{item.report ? <><div className="score-grid"><div><small>BASELINE</small><strong>{Math.round(item.report.baseline.successRate * 100)}%</strong><em>{Math.round(item.report.baseline.averageTokens)} tok</em></div><div><small>CANDIDATE</small><strong>{Math.round(item.report.candidate.successRate * 100)}%</strong><em>{Math.round(item.report.candidate.averageTokens)} tok</em></div><div><small>PAIRED DELTA</small><strong>+{item.report.frontierWins} / -{item.report.regressions}</strong><em>wins / regressions</em></div></div><p>{item.report.recommendationCause}</p>{item.report.recommendation === 'promote' && <button className="promote-button" disabled={!!busy} onClick={() => promote(item)}>{busy === `promote:${item.id}` ? 'Promoting…' : 'Promote candidate to stable'}</button>}</> : <div className="experiment-running"><i/><span>{item.lastError || 'Trials are running in the restricted evaluation scope…'}</span></div>}</article>)}{!relevantExperiments.length && <div className="ledger-empty">No evaluation evidence yet.</div>}</section>
        </>}
      </main>
    </div>
  </section></div>;
}
