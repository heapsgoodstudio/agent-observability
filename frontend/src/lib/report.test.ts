import { describe, expect, it } from 'vitest';
import type { Report, ReportAction, ReportDriver } from '../gen/claudeobs/v1/obs_pb';
import {
  actionMeta,
  avoidableText,
  costLabel,
  driverRows,
  hasReport,
  topAction,
  verdictLabel,
  VERDICT_TONE,
} from './report';

function report(over: Partial<Report> = {}): Report {
  return {
    $typeName: 'claudeobs.v1.Report',
    schema: 1, allowanceAware: false, verdict: '', avoidable: '', headline: 'H', summary: 'S', coverage: '',
    story: [], costDrivers: [], problems: [], good: [], actions: [], configChanges: [], fine: [],
    ...over,
  };
}

function driver(over: Partial<ReportDriver> = {}): ReportDriver {
  return {
    $typeName: 'claudeobs.v1.ReportDriver',
    label: 'L', amountUsd: 0, sharePct: 0, plain: '',
    ...over,
  };
}

function action(over: Partial<ReportAction> = {}): ReportAction {
  return {
    $typeName: 'claudeobs.v1.ReportAction', verification: '',
    doThis: 'D', why: '', effort: '', saving: '',
    ...over,
  };
}

describe('hasReport', () => {
  it('is false without a report', () => {
    expect(hasReport(undefined)).toBe(false);
  });

  it('is false when either field the surfaces need is blank', () => {
    expect(hasReport(report({ headline: '' }))).toBe(false);
    expect(hasReport(report({ summary: '   ' }))).toBe(false);
  });

  it('is true with both', () => {
    expect(hasReport(report())).toBe(true);
  });
});

describe('verdict and band wording', () => {
  it('keeps danger out of the report tones', () => {
    expect(Object.values(VERDICT_TONE)).not.toContain('danger');
  });

  it('words a known verdict and drops an unknown one', () => {
    expect(verdictLabel('wasteful')).toBe('wasteful');
    expect(verdictLabel('')).toBeNull();
    expect(verdictLabel('catastrophic')).toBeNull();
  });

  it('words the avoidable share', () => {
    expect(avoidableText('large')).toBe('a large part of this looks avoidable');
    expect(avoidableText('none')).toBe('nothing here looks avoidable');
    expect(avoidableText('')).toBeNull();
  });

  it('words a cost band', () => {
    expect(costLabel('moderate')).toBe('moderate usage impact');
    expect(costLabel('')).toBeNull();
  });
});

describe('actionMeta', () => {
  it('joins effort and saving', () => {
    expect(actionMeta(action({ effort: 'low', saving: 'large' }))).toBe('low effort · saves a lot');
  });

  it('keeps the half it has', () => {
    expect(actionMeta(action({ effort: 'high', saving: '' }))).toBe('high effort');
  });

  it('is null with neither', () => {
    expect(actionMeta(action())).toBeNull();
  });
});

describe('driverRows', () => {
  it('scales bars to the largest share and prints the true share', () => {
    const rows = driverRows([
      driver({ label: 'big', sharePct: 80, amountUsd: 1234.56 }),
      driver({ label: 'small', sharePct: 20, amountUsd: 452.33 }),
    ]);
    expect(rows[0].pct).toBe(100);
    expect(rows[0].share).toBe('80%');
    expect(rows[1].pct).toBe(25);
    expect(rows[1].share).toBe('20%');
  });

  it('lists labels with empty tracks when every share is zero', () => {
    const rows = driverRows([driver({ label: 'unknown share' })]);
    expect(rows).toHaveLength(1);
    expect(rows[0].pct).toBe(0);
    expect(rows[0].amount).toBe('');
  });

  it('is empty for no drivers', () => {
    expect(driverRows([])).toEqual([]);
  });
});

describe('topAction', () => {
  it('is the first action', () => {
    expect(topAction(report({ actions: [action({ doThis: 'first' }), action({ doThis: 'second' })] }))?.doThis).toBe('first');
  });

  it('is null with no actions', () => {
    expect(topAction(report())).toBeNull();
  });
});
