export type Provider = 'claude' | 'codex' | 'omp' | 'opencode';
export type ProviderFilter = '' | Provider;

export function normalizeProvider(value: string | null | undefined): ProviderFilter {
  return value === 'claude' || value === 'codex' || value === 'omp' || value === 'opencode' ? value : '';
}

// Rows indexed before provider attribution are Claude records.
export function providerOf(row: { provider?: string }): Provider {
  if (row.provider === 'codex') return 'codex';
  if (row.provider === 'omp') return 'omp';
  if (row.provider === 'opencode') return 'opencode';
  return 'claude';
}

// The one display name per provider, harness or inference provider alike.
// internal/profiles.ProviderName spells the harness names the server uses
// (Health rows, derived profile names) and must agree.
const PROVIDER_NAMES: Record<string, string> = { claude: 'Claude', codex: 'Codex', omp: 'Oh My Pi', opencode: 'opencode', baseten: 'Baseten', fireworks: 'Fireworks AI', lms: 'LM Studio', 'lm-studio': 'LM Studio' };

export function providerName(provider: string): string {
  return PROVIDER_NAMES[provider] ?? provider;
}

// The one mark per provider, beside its name in the sidebar, account rows and
// Settings. Oh My Pi's is its own pi.
const PROVIDER_MARKS: Record<string, string> = { claude: '✳', codex: '◉', omp: 'π', opencode: '◇' };

export function providerMark(provider: string): string {
  return PROVIDER_MARKS[provider] ?? provider.slice(0, 1).toUpperCase();
}

// The inference providers a detected-harness session sent requests to, by
// display name. internal/omp and internal/opencode name each model "<provider>/<model>", so they are the
// prefixes, in first-seen order.
export function inferenceProviders(row: { models: { model: string }[] }): string[] {
  const seen: string[] = [];
  for (const { model } of row.models) {
    const at = model.indexOf('/');
    const name = at > 0 ? providerName(model.slice(0, at)) : '';
    if (name && !seen.includes(name)) seen.push(name);
  }
  return seen;
}

// A detected-provider harness (Oh My Pi, opencode) has no allowance and no
// sign-in: its accounts are the pay-as-you-go inference providers found in its
// data, it asks for no profile name, and its dollars are its own estimates.
// internal/profiles.Detected is the server's half and must agree.
export function detectedHarness(provider: string): boolean {
  return provider === 'omp' || provider === 'opencode';
}

// Rows indexed before provider attribution carry no provider and are Claude.
export function providerLabel(provider: string): string {
  return providerName(provider || 'claude');
}

// Codex, Oh My Pi and opencode are indexed providers: local session records read from
// disk, outside the Claude plan allowance, with no generated analysis.
export function indexedProvider(provider: Provider): boolean {
  return provider === 'codex' || detectedHarness(provider);
}

export function sessionCostKnown(row: { provider?: string; costKnown?: boolean }): boolean {
  return providerOf(row) === 'claude' || row.costKnown === true;
}

export function sessionUsageKnown(row: { provider?: string; usageKnown?: boolean }): boolean {
  return providerOf(row) === 'claude' || row.usageKnown === true;
}

/** An incomplete aggregate reports only the observed portion. Missing values
 * cannot prove a zero; positive observations remain useful as a lower bound. */
export function observedTotal(value: number, complete: boolean, format: (value: number) => string): string {
  if (complete) return format(value);
  return value > 0 ? `${format(value)}+` : 'Unavailable';
}

/** Preserve both filters and existing query/hash when moving between views. */
export function withProfile(href: string, profile: string, provider: ProviderFilter = ''): string {
  const hashAt = href.indexOf('#');
  const hash = hashAt >= 0 ? href.slice(hashAt) : '';
  const base = hashAt >= 0 ? href.slice(0, hashAt) : href;
  const [path, existing] = base.split('?');
  const params = new URLSearchParams(existing ?? '');
  if (profile) params.set('profile', profile);
  if (provider) params.set('provider', provider);
  const query = params.toString();
  return `${path}${query ? `?${query}` : ''}${hash}`;
}

export const PROFILES_CHANGED_EVENT = 'agent-observability:profiles-changed';
// Fired when the collection a profile change started has finished, so every
// poll refetches data that did not exist when the change itself was made.
export const COLLECTION_FINISHED_EVENT = 'agent-observability:collection-finished';
