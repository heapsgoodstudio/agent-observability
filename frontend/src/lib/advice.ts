// Advice depth ordering and evidence scope. Timeouts come from GetMeta.
const PREFERRED_DEPTH_ORDER = ['standard', 'shallow', 'evidence'];

// The depth picker renders standard, shallow, evidence in that fixed order:
// standard first because it is the default, not because it sorts first.
// GetMeta.adviceDepths itself arrives alphabetical
// (internal/server/server.go's sortedKeys(analysis.Depths)), so this
// re-orders it back, appending any depth outside the known three
// alphabetically after them, the same convention sessionDetail.ts's
// orderAnalysisModels uses for the model picker.
export function orderAdviceDepths(depths: string[]): string[] {
  const known = PREFERRED_DEPTH_ORDER.filter((d) => depths.includes(d));
  const rest = depths.filter((d) => !PREFERRED_DEPTH_ORDER.includes(d)).sort();
  return [...known, ...rest];
}

// The initially-selected depth: standard when offered.
export function defaultAdviceDepth(depths: string[]): string {
  const ordered = orderAdviceDepths(depths);
  return ordered.includes('standard') ? 'standard' : ordered[0];
}

export interface DepthInfo {
  reads: string;
  cost: string;
}

export const DEPTH_INFO: Record<string, DepthInfo> = {
  standard: {
    reads: 'reviews up to 200 recent session summaries and relevant source configuration',
    cost: 'Broad usage review',
  },
  shallow: {
    reads: 'reviews scoped findings and up to 200 recent session summaries',
    cost: 'Focused usage review',
  },
  evidence: {
    reads: 'adds transcript evidence from up to ten recent sessions; reports sample limits',
    cost: 'Review with transcript evidence',
  },
};
