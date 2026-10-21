import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { ALIASES } from './aliases';

// Closes the gap CLAUDE.md's "Alert deep links are route aliases" entry
// names: aliases.test.ts asserts the ALIASES mapping itself, but nothing
// asserted that the scrollTo id each alias names actually exists as a
// Section on its target page, so a renamed Section id would be a silent
// no-op in scrollTarget.ts's useScrollTarget (which does nothing more than
// document.getElementById(id)?.scrollIntoView(...)) and no test would catch
// it.
//
// Every page component is 'use client' and fetches over Connect from a
// useEffect on mount, so none of them can be safely imported and rendered in
// a plain node test. Each target page is therefore checked by matching
// against its source text rather than by rendering it.
const PAGE_COMPONENT: Record<string, string> = {
  overview: 'Overview.tsx',
  spend: 'Spend.tsx',
  sessions: 'Sessions.tsx',
  findings: 'Insights.tsx',
  advice: 'Insights.tsx',
  health: 'Health.tsx',
};

function pageSource(page: string): string {
  const file = PAGE_COMPONENT[page];
  if (!file) {
    throw new Error(`no scroll-target check wired up for page "${page}"; add one rather than skipping it`);
  }
  return readFileSync(new URL(`../components/pages/${file}`, import.meta.url), 'utf8');
}

function pageHasSectionId(page: string, id: string): boolean {
  return new RegExp(`<Section\\s+id="${id}"`).test(pageSource(page));
}

describe('every alias scrollTo names a Section id that exists on its target page', () => {
  const targets = Object.entries(ALIASES).filter(([, a]) => a.scrollTo !== null);

  it('has at least one alias with a scrollTo to check', () => {
    expect(targets.length).toBeGreaterThan(0);
  });

  for (const [hash, { page, scrollTo }] of targets) {
    it(`${hash} resolves to ${page}'s "${scrollTo}" section`, () => {
      expect(pageHasSectionId(page, scrollTo as string)).toBe(true);
    });
  }
});
