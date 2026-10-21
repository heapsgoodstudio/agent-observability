import { describe, expect, it } from 'vitest';
import { resolveAlias } from './aliases';

// bin/usage-alert.sh posts #now, #overage and #history to the live
// dashboard, and breaking any of them sends an alert click to a blank page.
// Every key in ALIASES gets its own assertion here, by name, so a future
// edit that drops or mistypes one fails a specific test rather than a
// generic loop.

describe('resolveAlias', () => {
  it('resolves the empty and bare-hash aliases to overview', () => {
    expect(resolveAlias('')).toEqual({ path: '/', scrollTo: null });
    expect(resolveAlias('#')).toEqual({ path: '/', scrollTo: null });
  });

  it('resolves #overview to overview', () => {
    expect(resolveAlias('#overview')).toEqual({ path: '/', scrollTo: null });
  });

  it('resolves #spend to spend', () => {
    expect(resolveAlias('#spend')).toEqual({ path: '/spend', scrollTo: null });
  });

  it('resolves #sessions to sessions', () => {
    expect(resolveAlias('#sessions')).toEqual({ path: '/sessions', scrollTo: null });
  });

  it('resolves #insights and #findings to the insights view', () => {
    expect(resolveAlias('#insights')).toEqual({ path: '/insights', scrollTo: null });
    expect(resolveAlias('#findings')).toEqual({ path: '/insights', scrollTo: null });
  });

  it('resolves #advice to the insights advice tab', () => {
    expect(resolveAlias('#advice')).toEqual({ path: '/insights?tab=advice', scrollTo: null });
  });

  it('resolves #health to health', () => {
    expect(resolveAlias('#health')).toEqual({ path: '/health', scrollTo: null });
  });

  // The three alert deep links bin/usage-alert.sh actually posts, asserted
  // explicitly and individually: these are the ones that break a real
  // notification click if ALIASES drifts.
  describe('the alert deep links', () => {
    it('#now scrolls overview to its meters section', () => {
      expect(resolveAlias('#now')).toEqual({ path: '/', scrollTo: 'meters' });
    });

    it('#overage scrolls overview to its curve section', () => {
      expect(resolveAlias('#overage')).toEqual({ path: '/', scrollTo: 'curve' });
    });

    it('#history scrolls spend to its daily section', () => {
      expect(resolveAlias('#history')).toEqual({ path: '/spend', scrollTo: 'daily' });
    });
  });

  it('returns null for an unknown hash', () => {
    expect(resolveAlias('#nonsense')).toBeNull();
    expect(resolveAlias('#sessions/not-an-alias')).toBeNull();
  });

  it('strips a trailing query string before matching', () => {
    expect(resolveAlias('#spend?period=7d')).toEqual({ path: '/spend', scrollTo: null });
  });

  it('matches case-insensitively', () => {
    expect(resolveAlias('#HISTORY')).toEqual({ path: '/spend', scrollTo: 'daily' });
  });
});
