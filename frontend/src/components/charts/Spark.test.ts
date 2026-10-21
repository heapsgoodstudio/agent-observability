import { describe, expect, it } from 'vitest';
import { Spark } from './Spark';

// Cheap render test: Spark is a plain function component, so calling it
// directly returns the React element object without needing jsdom or
// react-dom (neither is a dependency of this frontend). Asserts the class
// difference between the two Spark sizes: tile (the default) and large
// (used by the session-detail trajectory charts).

const points: Array<[number, number]> = [
  [0, 1],
  [1, 2],
  [2, 1.5],
];

describe('Spark size prop', () => {
  it('defaults to the tile size: fixed 28px height, 120px max-width', () => {
    const el = Spark({ points, label: 'tile trend' }) as unknown as {
      props: { className: string };
    };
    expect(el.props.className).toContain('h-[28px]');
    expect(el.props.className).toContain('max-w-[120px]');
    expect(el.props.className).not.toContain('h-[100px]');
  });

  it('size="large" renders 100px height with no max-width', () => {
    const el = Spark({ points, label: 'large trend', size: 'large' }) as unknown as {
      props: { className: string };
    };
    expect(el.props.className).toContain('h-[100px]');
    expect(el.props.className).not.toContain('max-w-[120px]');
    expect(el.props.className).not.toContain('h-[28px]');
  });
});
