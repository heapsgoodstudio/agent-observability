import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { create } from '@bufbuild/protobuf';
import { describe, expect, it } from 'vitest';
import { AnalysisModelSchema, AnalysisTargetSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { EvaluatorSelect, ModelDropdown, initialEvaluator, evaluatorAvailable, selectEvaluatorProfile } from './EvaluatorSelect';

describe('evaluator selection', () => {
  const targets = [
    create(AnalysisTargetSchema, { provider: 'claude', profile: 'disabled', available: false }),
    create(AnalysisTargetSchema, { provider: 'codex', profile: 'work', name: 'Work', account: 'acct', available: true }),
    create(AnalysisTargetSchema, { provider: 'omp', profile: 'personal', name: 'Personal', available: true }),
  ];
  it('selects an available profile without tying it to source', () => {
    const selection = initialEvaluator(targets);
    expect(selection).toEqual({ provider: 'codex', profile: 'work', model: '' });
    expect(evaluatorAvailable(targets, { ...selection, model: 'custom/provider-model' })).toBe(true);
    expect(evaluatorAvailable(targets, { ...selection, profile: 'personal' })).toBe(false);
  });
  it('renders account provenance and a model dropdown with all four providers', () => {
    const html = renderToStaticMarkup(<EvaluatorSelect targets={targets} value={initialEvaluator(targets)} onChange={() => {}} />);
    expect(html).toContain('Analysis account or profile');
    expect(html).toContain('Work · acct');
    expect(html).toContain('value="opencode"');
    expect(html).toContain('Analysis model');
    expect(html).toContain('Profile default');
    expect(html).toContain('<select aria-label="Analysis model"');
    expect(html).not.toContain('<input');
  });
  it('clears the selected model when switching credential profiles', () => {
    expect(selectEvaluatorProfile({ provider: 'codex', profile: 'one', model: 'account-specific-model' }, 'two'))
      .toEqual({ provider: 'codex', profile: 'two', model: '' });
  });
  it('groups configured harness models by inference provider and submits exact IDs', () => {
    const models = [
      create(AnalysisModelSchema, { id: 'vendor/org/model', name: 'Configured model', provider: 'vendor' }),
      create(AnalysisModelSchema, { id: 'local/custom', name: 'Custom', provider: 'local' }),
    ];
    const html = renderToStaticMarkup(<ModelDropdown models={models} value="vendor/org/model" onChange={() => {}} loading={false} error={null} />);
    expect(html).toContain('<optgroup label="vendor">');
    expect(html).toContain('value="vendor/org/model" selected=""');
    expect(html).toContain('Configured model');
    expect(html).not.toContain('<input');
  });
  it('keeps profile default available during discovery and after failure', () => {
    for (const props of [{ loading: true, error: null }, { loading: false, error: 'unavailable' }]) {
      const html = renderToStaticMarkup(<ModelDropdown models={[]} value="" onChange={() => {}} {...props} />);
      expect(html).toContain('value="" selected="">Profile default');
      expect(html).not.toContain('disabled=');
      expect(html).toContain('role="status"');
    }
  });
});
