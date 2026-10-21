'use client';

import { useCallback } from 'react';
import type { AnalysisModel, AnalysisTarget } from '@/src/gen/claudeobs/v1/obs_pb';
import { obsClient } from '@/src/lib/client';
import { usePoll } from '@/src/lib/usePoll';
import { providerLabel } from '@/src/lib/providers';

export type Evaluator = { provider: string; profile: string; model: string };
export function initialEvaluator(targets: AnalysisTarget[]): Evaluator {
  const target = targets.find((t) => t.available);
  return { provider: target?.provider ?? '', profile: target?.profile ?? '', model: '' };
}
export function evaluatorAvailable(targets: AnalysisTarget[], value: Evaluator): boolean {
  return targets.some((t) => t.provider === value.provider && t.profile === value.profile && t.available);
}
const CONTROL = 'obs-input w-full min-w-0';

export function selectEvaluatorProfile(value: Evaluator, profile: string): Evaluator {
  return { ...value, profile, model: '' };
}

export function ModelDropdown({ models, value, onChange, loading, error }: {
  models: AnalysisModel[]; value: string; onChange: (model: string) => void; loading: boolean; error: string | null;
}) {
  const groups = [...new Set(models.map((m) => m.provider))];
  const option = (model: AnalysisModel) => <option key={model.id} value={model.id}>{model.name === model.id ? model.id : `${model.name} · ${model.id}`}</option>;
  return <label className="flex min-w-0 flex-col gap-1 text-[12px] text-muted">Model
    <select aria-label="Analysis model" aria-busy={loading} className={CONTROL} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">Profile default</option>
      {value && !models.some((m) => m.id === value) ? <option value={value}>Previously selected · {value}</option> : null}
      {groups.map((group) => group ? <optgroup key={group} label={group}>{models.filter((m) => m.provider === group).map(option)}</optgroup> : models.filter((m) => !m.provider).map(option))}
    </select>
    {loading ? <span role="status">Loading configured models…</span> : error ? <span role="status">Could not load models. You can still use the profile default.</span> : !models.length ? <span>No models were listed. You can still use the profile default.</span> : null}
  </label>;
}

export function EvaluatorSelect({ targets, value, onChange, disabled = false }: {
  targets: AnalysisTarget[]; value: Evaluator; onChange: (value: Evaluator) => void; disabled?: boolean;
}) {
  const target = targets.find((t) => t.provider === value.provider && t.profile === value.profile);
  const available = !!target?.available;
  const catalog = usePoll(useCallback(async () => available
    ? obsClient.getAnalysisModels({ provider: value.provider, profile: value.profile })
    : null, [available, value.provider, value.profile]), 60000);
  const providers = ['claude', 'codex', 'omp', 'opencode'];
  return <fieldset disabled={disabled} className="m-0 min-w-0 border-0 p-0">
    <legend className="mb-2 text-[12px] font-medium text-secondary">Run analysis with</legend>
    <div className="grid gap-2 sm:grid-cols-3">
      <label className="flex min-w-0 flex-col gap-1 text-[12px] text-muted">Provider
        <select aria-label="Analysis provider" className={CONTROL} value={value.provider} onChange={(e) => {
          const next = targets.find((t) => t.provider === e.target.value && t.available) ?? targets.find((t) => t.provider === e.target.value);
          onChange({ provider: e.target.value, profile: next?.profile ?? '', model: '' });
        }}>
          {!value.provider ? <option value="">Select provider</option> : null}
          {providers.map((p) => <option value={p} key={p}>{providerLabel(p)}</option>)}
        </select>
      </label>
      <label className="flex min-w-0 flex-col gap-1 text-[12px] text-muted">Account / profile
        <select aria-label="Analysis account or profile" className={CONTROL} value={value.profile} onChange={(e) => onChange(selectEvaluatorProfile(value, e.target.value))}>
          {!value.profile ? <option value="">Add a profile in Settings</option> : null}
          {targets.filter((t) => t.provider === value.provider).map((t) => <option key={t.profile} value={t.profile} disabled={!t.available}>
            {t.name || t.profile}{t.account ? ` · ${t.account}` : ' · configured credentials'}{!t.available ? ` · ${t.unavailableReason}` : ''}
          </option>)}
        </select>
      </label>
      <ModelDropdown models={catalog.data?.models ?? []} value={value.model} onChange={(model) => onChange({ ...value, model })} loading={available && !catalog.fetchedAt && !catalog.error} error={catalog.error} />
    </div>
    <p className="m-0 mt-2 text-[12px] text-muted">Models come from the selected profile’s CLI configuration. This run sends the selected evidence to this account and uses its allowance or API billing.</p>
    {target && !target.available ? <p role="status" className="mt-2 text-[12px] text-muted">{target.unavailableReason}</p> : null}
  </fieldset>;
}
