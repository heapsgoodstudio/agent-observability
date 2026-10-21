import { describe, expect, it } from 'vitest';
import { nextTheme, THEMES, type Theme } from './theme';

describe('nextTheme', () => {
  it('cycles through every theme and back to the start', () => {
    // Explicitly typed as Theme, not inferred from THEMES[0]: TypeScript
    // narrows an array literal's indexed access to that element's own
    // literal type ("system"), not the union it belongs to, so the
    // reassignment below (nextTheme returns the wider Theme) fails to
    // typecheck against the narrower inferred type without this annotation.
    let theme: Theme = THEMES[0];
    for (let i = 0; i < THEMES.length; i++) {
      theme = nextTheme(theme);
    }
    expect(theme).toBe(THEMES[0]);
  });
});
