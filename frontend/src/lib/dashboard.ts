import { CLAUDE_FRESH_FOR_S, CODEX_FRESH_FOR_S, allowanceTone, codexPlanLabel } from './allowance';
import { agoText, compactInt } from './format';
import { detectedHarness, providerName } from './providers';
import type { AccountSnapshot, AllowanceWindow, BillingSnapshot, Point, Band } from '@/src/gen/claudeobs/v1/obs_pb';

export type DashboardScope = { provider: string; accountKey: string; profile: string };
export const ALL_SCOPE: DashboardScope = { provider: '', accountKey: '', profile: '' };
export function scopeHref(path: string, scope: DashboardScope): string {
  const [base, hash] = path.split('#');
  const [route, query] = base.split('?');
  const params = new URLSearchParams(query);
  for (const [key, value] of Object.entries(scope)) {
    if (value) params.set(key, value); else params.delete(key);
  }
  return `${route}${params.size ? `?${params}` : ''}${hash ? `#${hash}` : ''}`;
}
export function accountScope(account: AccountSnapshot): DashboardScope {
  return { provider: account.provider, accountKey: account.key, profile: '' };
}
export function matchesAccountScope(account: AccountSnapshot, scope: DashboardScope): boolean {
  return (!scope.accountKey || account.key === scope.accountKey) && (!scope.provider || account.provider === scope.provider) && (!scope.profile || account.profiles.some((p) => p.profile === scope.profile));
}
export function accountLabel(account: AccountSnapshot): string {
  return account.displayName || account.account || account.profiles[0]?.displayName || 'Awaiting account identity';
}
export function accountPlan(account: AccountSnapshot): string {
  return account.plan ? account.provider === 'codex' ? codexPlanLabel(account.plan) : account.plan : '';
}
export { providerName };
export const known = (value: number | undefined): value is number => value !== undefined && Number.isFinite(value);
export function windowRoles(account: AccountSnapshot) {
  const current = account.windows.find((w) => w.id === account.currentWindowId);
  const weekly = account.windows.find((w) => w.id === account.weeklyWindowId);
  return { current, weekly, shared: !!current && current.id === weekly?.id, extra: account.windows.filter((w) => w.id !== current?.id && w.id !== weekly?.id) };
}
// Claude cache observedAt is the last value change. Only confirmedAt answers
// whether those values were checked; missing confirmation remains unknown.
export function windowState(window: AllowanceWindow, now: number): string {
  if (known(window.resetsAt) && window.resetsAt <= now) return 'Awaiting reset confirmation';
  if (window.error) return 'Last poll failed';
  if (window.state === 'unavailable') return 'Reading unavailable';
  if (!known(window.confirmedAt) || window.confirmedAt > now + 60) return known(window.observedAt) || known(window.usedPercent) ? 'Confirmation unavailable' : 'Reading unavailable';
  const ttl = window.source.toLowerCase().includes('codex') ? CODEX_FRESH_FOR_S : CLAUDE_FRESH_FOR_S;
  if (window.state === 'stale' || now - window.confirmedAt > ttl) return 'Stale reading';
  return window.state === 'fresh' ? 'Confirmed' : 'Confirmation unavailable';
}

export type AccountFreshness = { state: string; label: string; at?: number; timeLabel: string; detail: string };
export function accountFreshness(account: AccountSnapshot, now: number): AccountFreshness {
  if (detectedHarness(account.provider)) return spendFreshness(account, now);
  const ttl = account.provider === 'codex' ? CODEX_FRESH_FOR_S : CLAUDE_FRESH_FOR_S;
  const windows = account.windows.map((window) => ({ window, status: windowState(window, now) }));
  const confirmations = windows.map(({ window }) => window.confirmedAt).filter((at): at is number => known(at) && at <= now + 60);
  // Derive this from the windows too, so retained responses with the old
  // max-confirmation aggregate cannot hide an older model-specific reading.
  const oldest = windows.length ? confirmations.length === windows.length ? Math.min(...confirmations) : undefined : known(account.confirmedAt) && account.confirmedAt <= now + 60 ? account.confirmedAt : undefined;
  const detail = `${providerName(account.provider)} confirmations are fresh for ${ttl / 60} minutes. ${windows.map(({ window, status }) => `${window.label || window.id}: ${status}`).join('; ')}`.trim();
  const base = { at: oldest, timeLabel: windows.length > 1 ? 'oldest confirmation' : 'last confirmed', detail };
  if (account.state === 'unavailable') return { ...base, state: 'unavailable', label: 'Unavailable' };
  if (account.error || windows.some(({ status }) => status === 'Last poll failed')) return { ...base, state: 'stale', label: 'Poll failed', detail: `${detail} ${account.error}`.trim() };
  const stale = windows.filter(({ status }) => status === 'Stale reading');
  if (stale.length) {
    const times = stale.map(({ window }) => window.confirmedAt).filter(known);
    return { ...base, at: times.length ? Math.min(...times) : undefined, timeLabel: stale.length > 1 ? 'oldest stale confirmation' : 'last confirmed', state: 'stale', label: stale.length === windows.length ? 'Stale' : `${stale.length} of ${windows.length} windows stale` };
  }
  const unconfirmed = windows.filter(({ status }) => status === 'Confirmation unavailable' || status === 'Reading unavailable');
  if (unconfirmed.length) return { ...base, at: undefined, state: 'unknown', label: unconfirmed.length === windows.length ? 'Confirmation unknown' : `${unconfirmed.length} of ${windows.length} windows unconfirmed` };
  if (windows.some(({ status }) => status === 'Awaiting reset confirmation')) return { ...base, state: 'stale', label: 'Reset confirmation pending' };
  if (!known(oldest)) return { ...base, state: 'unknown', label: known(account.observedAt) ? 'Confirmation unknown' : 'Awaiting data' };
  if (now - oldest > ttl) return { ...base, state: 'stale', label: 'Stale' };
  if (account.state === 'stale') return { ...base, state: 'stale', label: 'Refresh pending' };
  return { ...base, state: account.state, label: account.state === 'fresh' ? 'Confirmed' : 'Confirmation unknown' };
}

// Detected-harness accounts (Oh My Pi, opencode) have no provider confirming
// anything: their figures are read from local data, collected every five
// minutes and counted stale after fifteen, the server's own session-index
// threshold. So they say "Collected" where allowance accounts say "Confirmed".
function spendFreshness(account: AccountSnapshot, now: number): AccountFreshness {
  const at = known(account.confirmedAt) && account.confirmedAt <= now + 60 ? account.confirmedAt : undefined;
  const base = { at, timeLabel: 'last collected', detail: `${providerName(account.provider)} usage is read from local data every five minutes and counts as stale after fifteen.` };
  if (account.error) return { ...base, state: 'stale', label: 'Collection failed', detail: `${base.detail} ${account.error}` };
  if (!known(at)) return { ...base, state: 'unknown', label: 'Awaiting collection' };
  if (now - at > 900 || account.state === 'stale') return { ...base, state: 'stale', label: 'Stale' };
  return { ...base, state: 'fresh', label: 'Collected' };
}

// Show exact minute/second ages near freshness boundaries instead of
// rounding a reading past the limit back down to the limit.
export function confirmationAge(at: number, now: number, precise = false): string {
  const seconds = Math.max(0, Math.floor(now - at));
  if (!precise || seconds >= 3600) return agoText(at, now);
  return seconds < 60 ? `${seconds}s ago` : `${Math.floor(seconds / 60)}m ${seconds % 60}s ago`;
}
// The sidebar's groups: every provider with an account, plus every detected
// harness (Oh My Pi, opencode) with an enabled profile but no account yet. A
// detected harness's accounts only appear with its first request, so without
// this a newly added profile would be invisible in the sidebar until then.
// Accounts hidden in Settings are left out and counted, so a group whose
// accounts are all hidden still says so rather than vanishing.
export function sidebarGroups(accounts: AccountSnapshot[], profiles: { provider: string; enabled: boolean }[]): { provider: string; accounts: AccountSnapshot[]; hidden: number }[] {
  const providers = new Set(accounts.map((a) => a.provider));
  for (const p of profiles) if (p.enabled && detectedHarness(p.provider)) providers.add(p.provider);
  return [...providers].sort().map((provider) => {
    const all = accounts.filter((a) => a.provider === provider);
    return { provider, accounts: all.filter((a) => !a.hidden), hidden: all.filter((a) => a.hidden).length };
  });
}

// Whether a pay-as-you-go account has any recorded cost. One without (a local
// model, or one its harness cannot price) shows token usage instead of money.
export function costRecorded(account: AccountSnapshot): boolean {
  return account.capabilities.includes('estimated_spend');
}

export function tokensLabel(tokens: number): string {
  return `${compactInt(tokens)} tok`;
}

// The one figure the sidebar shows per account: allowance used for plan
// accounts, estimated spend this month for pay-as-you-go ones. An account
// whose current and weekly windows are different windows (Claude's 5-hour and
// weekly) shows both, current first; one shared window (Codex) shows once.
// Two bare percentages do not say which is which, so the title names them.
// Colour each percentage by proximity to its cap. Freshness belongs in the
// overview; stale readings keep the same sidebar presentation as fresh ones.
export function sidebarFigure(account: AccountSnapshot): { text: string; title?: string; windows?: { id: string; text: string; tone: ReturnType<typeof allowanceTone> }[] } {
  if (account.state !== 'stale' && account.state !== 'fresh') return { text: '—' };
  if (detectedHarness(account.provider)) {
    if (!costRecorded(account)) return { text: known(account.monthToDateTokens) ? tokensLabel(account.monthToDateTokens) : '—' };
    return { text: known(account.billing?.estimatedOverageUsd) ? billingLabel(account.billing, true) : '—' };
  }
  const { current, weekly, shared } = windowRoles(account);
  const shown = current && weekly && !shared ? [current, weekly] : current ? [current] : [];
  const pct = (w: AllowanceWindow) => {
    const used = w.usedPercent;
    return known(used) ? `${Math.round(used)}%` : '—';
  };
  if (!shown.length) return { text: '—' };
  return {
    text: shown.map(pct).join(' / '),
    title: shown.map((w) => `${w.label || w.id}: ${pct(w)} used`).join('\n'),
    windows: shown.map((w) => ({ id: w.id, text: pct(w), tone: allowanceTone(w.usedPercent) })),
  };
}

// The workspace total includes reported charges, recorded pay-as-you-go
// estimates and reported credits at a configured price, once per account
// (including hidden accounts). Reported amounts take precedence. Estimates
// remain visibly labelled and missing prices are never silently turned into a
// complete zero-dollar total.
export function billedTotal(accounts: AccountSnapshot[]): { text: string; stale: boolean; title: string; estimated: boolean } | null {
  const unique = [...new Map(accounts.map((a) => [a.key || `${a.provider}:${a.account}`, a])).values()];
  const usd = estimateLabel;
  let total = 0, count = 0;
  let harnessEstimated = false, creditEstimated = false, partial = false, stale = false;
  const lines: string[] = [];
  const off: string[] = [];
  for (const account of unique) {
    const billing = account.billing;
    const reported = known(billing?.reportedOverageUsd);
    const credits = !reported && known(billing?.reportedOverageCredits);
    const harness = !reported && detectedHarness(account.provider) && known(billing?.estimatedOverageUsd);
    const priced = credits && known(billing?.estimatedOverageUsd);
    const estimate = harness || priced;
    const name = `${providerName(account.provider)} · ${accountLabel(account)}`;
    if (reported || estimate) {
      const amount = reported ? billing!.reportedOverageUsd! : billing!.estimatedOverageUsd!;
      const incomplete = harness && billing?.estimatedOverageComplete === false;
      const old = billing?.state === 'stale' || account.state === 'stale' || !!account.error;
      total += amount;
      count++;
      harnessEstimated ||= harness;
      creditEstimated ||= priced;
      partial ||= incomplete;
      stale ||= old;
      const how = priced ? ` (estimated from ${creditsLabel(billing!.reportedOverageCredits!)})` : harness ? ' (estimated)' : '';
      lines.push(`${name}: ${usd(amount)}${incomplete ? '+' : ''}${how}${old ? ' (stale reading)' : ''}`);
    } else if (credits) {
      partial = true;
      lines.push(`${name}: ${creditsLabel(billing!.reportedOverageCredits!)}, no price per credit set`);
    } else if (billing?.enabled === false) {
      off.push(accountLabel(account));
    } else if (billing?.enabled === true || costRecorded(account)) {
      partial = true;
      lines.push(`${name}: amount unknown`);
    } else if (billing?.state === 'unavailable') {
      lines.push(`${name}: billing not provided`);
    }
  }
  const offLine = off.length ? `Extra usage off: ${off.join(', ')}` : '';
  if (!count) {
    if (partial) return { text: 'Unknown', stale: false, estimated: false, title: ['No billed amount is available yet.', ...lines, offLine].filter(Boolean).join('\n') };
    return off.length ? { text: 'Off', stale: false, estimated: false, title: [...lines, offLine].filter(Boolean).join('\n') } : null;
  }
  const estimated = harnessEstimated || creditEstimated;
  return {
    text: `${estimated ? '≈' : ''}${usd(total)}${partial ? '+' : ''}`, stale, estimated,
    title: ['Month-to-date usage charges across all accounts', ...lines, offLine,
      harnessEstimated ? 'Includes harness estimates for recorded pay-as-you-go requests; these are not provider invoices and exclude usage from other clients.' : '',
      creditEstimated ? 'Includes Codex credits priced at CODEX_CREDIT_USD; an estimate, not an invoice.' : '',
      partial ? '+ marks a partial total: some amounts, request prices or credit prices are unknown.' : '',
    ].filter(Boolean).join('\n'),
  };
}

// An estimated dollar figure. A sub-cent spend is real spend, and rounding it
// to $0.00 would read as none.
export function estimateLabel(value: number): string {
  return value > 0 && value < 0.01 ? '<$0.01' : new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' }).format(value);
}

// A provider's credit amount, in its own units, to the cent as Codex shows it.
export function creditsLabel(value: number): string {
  return `${new Intl.NumberFormat('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value)} credits`;
}

// A configured price per credit, to as many decimal places as it was set with.
export function creditPriceLabel(value: number): string {
  return new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 6 }).format(value);
}

export function billingLabel(billing: BillingSnapshot | undefined, showEstimate = false): string {
  if (known(billing?.reportedOverageUsd)) return new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' }).format(billing.reportedOverageUsd);
  // Codex reports its overage in credits. A configured price per credit turns
  // them into an estimate, which BillingValue labels as one.
  if (known(billing?.reportedOverageCredits)) return known(billing.estimatedOverageUsd) ? estimateLabel(billing.estimatedOverageUsd) : creditsLabel(billing.reportedOverageCredits);
  // An estimate is shown only when the caller opts in, which only a detected
  // harness's pay-as-you-go accounts do: no plan includes their usage. BillingValue
  // labels it as an estimate so it never reads as provider-reported.
  if (showEstimate && known(billing?.estimatedOverageUsd)) return estimateLabel(billing.estimatedOverageUsd);
  if (billing?.enabled === false) return 'Overage disabled';
  if (billing?.enabled === true) return 'Amount unknown';
  return billing?.state === 'unavailable' ? 'Not provided' : 'Unknown';
}
// Keep every period and explicit gap disconnected. Also split at decreases
// in cumulative observations; a reset/refund is not a negative daily bill.
export function historySegments(points: Pick<Point, 'ts' | 'value'>[], gaps: Pick<Band, 'start' | 'end'>[] = []): Pick<Point, 'ts' | 'value'>[][] {
  const sorted = points.filter((p) => Number.isFinite(p.ts) && Number.isFinite(p.value)).toSorted((a, b) => a.ts - b.ts);
  const segments: Pick<Point, 'ts' | 'value'>[][] = [];
  for (const point of sorted) {
    const segment = segments.at(-1);
    const previous = segment?.at(-1);
    if (!previous || point.value < previous.value || gaps.some((gap) => gap.start < point.ts && gap.end > previous.ts)) segments.push([point]);
    else if (point.ts !== previous.ts) segment!.push(point);
  }
  return segments;
}
