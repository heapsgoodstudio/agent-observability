import { describe, expect, it } from 'vitest';
import { niceCeil, ticks } from './scale';

describe('niceCeil', () => {
  it('rounds up to a half decade step', () => {
    expect(niceCeil(1)).toBe(1);
    expect(niceCeil(7)).toBe(7);
    expect(niceCeil(3412.5)).toBe(3500);
    expect(niceCeil(12)).toBe(15);
  });

  it('returns one for nothing positive', () => {
    expect(niceCeil(0)).toBe(1);
    expect(niceCeil(-5)).toBe(1);
    expect(niceCeil(NaN)).toBe(1);
  });
});

describe('ticks', () => {
  it('spans zero to max inclusive', () => {
    expect(ticks(100)).toEqual([0, 50, 100]);
    expect(ticks(100, 4)).toEqual([0, 25, 50, 75, 100]);
  });
});
