import { describe, expect, it } from 'vitest';
import { detectedHarness, indexedProvider, inferenceProviders, normalizeProvider, providerLabel, providerName, providerOf, sessionCostKnown, sessionUsageKnown, withProfile } from './providers';

describe('provider selection and session capabilities', () => {
  it('accepts only supported provider filters', () => {
    expect(normalizeProvider('codex')).toBe('codex');
    expect(normalizeProvider('claude')).toBe('claude');
    expect(normalizeProvider('omp')).toBe('omp');
    expect(normalizeProvider('unknown')).toBe('');
    expect(normalizeProvider(null)).toBe('');
  });

  it('preserves historical Claude attribution and availability', () => {
    expect(providerOf({ provider: '' })).toBe('claude');
    expect(sessionCostKnown({ provider: '', costKnown: false })).toBe(true);
    expect(sessionUsageKnown({ provider: '' })).toBe(true);
  });

  it('never invents a known zero from missing Codex cost or usage', () => {
    expect(sessionCostKnown({ provider: 'codex', costKnown: false })).toBe(false);
    expect(sessionUsageKnown({ provider: 'codex', usageKnown: false })).toBe(false);
    expect(sessionCostKnown({ provider: 'codex', costKnown: true })).toBe(true);
    expect(sessionUsageKnown({ provider: 'codex', usageKnown: true })).toBe(true);
  });

  it('recognises omp as its own provider, never folded into Claude or Codex', () => {
    expect(providerOf({ provider: 'omp' })).toBe('omp');
    expect(providerLabel('omp')).toBe('Oh My Pi');
    expect(providerLabel('')).toBe('Claude');
    expect(providerName('fireworks')).toBe('Fireworks AI');
    expect(providerName('wire')).toBe('wire');
    expect(providerName('lm-studio')).toBe('LM Studio');
    expect(providerName('lms')).toBe('LM Studio');
    expect(sessionCostKnown({ provider: 'omp', costKnown: false })).toBe(false);
    expect(sessionCostKnown({ provider: 'omp', costKnown: true })).toBe(true);
    expect(sessionUsageKnown({ provider: 'omp', usageKnown: false })).toBe(false);
    expect(sessionUsageKnown({ provider: 'omp', usageKnown: true })).toBe(true);
  });

  it('flags codex and omp as indexed providers, never Claude', () => {
    expect(indexedProvider('codex')).toBe(true);
    expect(indexedProvider('omp')).toBe(true);
    expect(indexedProvider('claude')).toBe(false);
  });
});

describe('navigation filters', () => {
  it('carries both filters across session details and tabs', () => {
    const path = withProfile('/sessions?id=codex%3Aabc&tab=analysis', 'codex-work', 'codex');
    const params = new URLSearchParams(path.split('?')[1]);
    expect(params.get('id')).toBe('codex:abc');
    expect(params.get('tab')).toBe('analysis');
    expect(params.get('profile')).toBe('codex-work');
    expect(params.get('provider')).toBe('codex');
  });

  it('preserves deep-link anchors and existing spend parameters', () => {
    expect(withProfile('/spend?period=7d#daily', 'personal', 'claude'))
      .toBe('/spend?period=7d&profile=personal&provider=claude#daily');
  });

  it('keeps unfiltered routes clean and encodes profile names', () => {
    expect(withProfile('/sessions', '', '')).toBe('/sessions');
    expect(withProfile('/sessions', 'my profile', 'codex')).toBe('/sessions?profile=my+profile&provider=codex');
  });
});

describe('inference providers', () => {
  it('lists the providers an Oh My Pi session used, once each, by display name', () => {
    const row = { models: [{ model: 'baseten/glm-5.3' }, { model: 'fireworks/glm-5.3-flash' }, { model: 'baseten/glm-5.3-flash' }, { model: 'claude-opus-5' }] };
    expect(inferenceProviders(row)).toEqual(['Baseten', 'Fireworks AI']);
  });
});

describe('opencode', () => {
  it('is a detected-provider harness like Oh My Pi, with its own fixed name', () => {
    expect(normalizeProvider('opencode')).toBe('opencode');
    expect(providerOf({ provider: 'opencode' })).toBe('opencode');
    expect(providerLabel('opencode')).toBe('opencode');
    expect(indexedProvider('opencode')).toBe(true);
    expect(detectedHarness('opencode')).toBe(true);
    expect(detectedHarness('omp')).toBe(true);
    expect(detectedHarness('codex')).toBe(false);
    expect(detectedHarness('claude')).toBe(false);
  });
});

