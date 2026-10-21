import { describe, expect, it } from 'vitest';
import { topRoundedRect } from './geometry';

// A rect path with rounded top corners only (the data-end of a
// stacked-column segment),
// square at the baseline. Extracted here so the path arithmetic is testable
// without rendering the Columns component.

describe('topRoundedRect', () => {
  it('draws a rect with the requested top radius', () => {
    expect(topRoundedRect(0, 0, 10, 20, 2)).toBe(
      'M 0 20 L 0 2 Q 0 0 2 0 L 8 0 Q 10 0 10 2 L 10 20 Z',
    );
  });

  it('clamps the radius to half the width and the full height', () => {
    // r=3 requested, but w/2=2 and h=1 are both smaller: rr must be 1.
    expect(topRoundedRect(5, 5, 4, 1, 3)).toBe(
      'M 5 6 L 5 6 Q 5 5 6 5 L 8 5 Q 9 5 9 6 L 9 6 Z',
    );
  });
});
