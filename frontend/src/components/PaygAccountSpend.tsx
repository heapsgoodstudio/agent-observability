'use client';

import { useCallback } from 'react';
import { create } from '@bufbuild/protobuf';
import { obsClient } from '@/src/lib/client';
import { PointSchema, type AccountSnapshot, type PaygProvider } from '@/src/gen/claudeobs/v1/obs_pb';
import { usePoll } from '@/src/lib/usePoll';
import { compactInt, fmtInt } from '@/src/lib/format';
import { accountLabel, costRecorded, estimateLabel, tokensLabel } from '@/src/lib/dashboard';
import { observedTotal, providerName } from '@/src/lib/providers';
import { Alert } from '@/src/components/AccountUI';
import { ObservationChart } from '@/src/components/charts/ObservationChart';

// Every dollar here is the harness's own estimate (Oh My Pi or opencode):
// provider-reported tokens priced at its catalog rates, never a bill. See
// docs/omp-provider.md and docs/opencode-provider.md.
const caption = (harness: string) => `${providerName(harness)}'s estimate: provider-reported tokens priced at public API rates, not a bill. Requests other clients sent with the same provider account are not included.`;
// A provider with no recorded cost is a local model, or one the harness has
// no price for; its data cannot tell the two apart, so this says neither.
const noCostCaption = (harness: string) => `${providerName(harness)} recorded no cost for these requests: a local model, or one it has no price for. Token counts are the provider's own.`;
const DAY = 86400;

// Local noon, so a YYYY-MM-DD day never lands on the previous local date.
function dayStart(day: string): number {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  return match ? new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3]), 12).getTime() / 1000 : 0;
}

function monthLabel(month: string): string | undefined {
  const match = /^(\d{4})-(\d{2})$/.exec(month);
  return match ? new Date(Number(match[1]), Number(match[2]) - 1, 1).toLocaleDateString(undefined, { month: 'long', year: 'numeric' }) : undefined;
}

function Stat({ label, value, note }: { label: string; value: string; note?: string }) {
  return <div><p className="obs-eyebrow mt-0">{label}</p><div className="obs-billing-value">{value}</div>{note ? <div className="obs-billing-note">{note}</div> : null}</div>;
}

function Spend({ provider, month, name, harness }: { provider: PaygProvider; month: string; name: string; harness: string }) {
  const costed = provider.costRecorded;
  const points = provider.days.map((d) => create(PointSchema, { ts: dayStart(d.day), value: costed ? d.costUsd : d.tokens })).filter((p) => p.ts > 0);
  const lastActive = provider.lastActiveDay ? new Date(dayStart(provider.lastActiveDay) * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' }) : 'Never';
  const figure = costed ? 'estimated spend' : 'tokens';
  return <>
    <section className="obs-panel" aria-label={`${name} ${figure}`}>
      <div className="obs-panel-body">
        <div className="grid gap-6 sm:grid-cols-3">
          {costed
            ? <><Stat label="Month to date" value={observedTotal(provider.monthToDateUsd, provider.monthToDateComplete, estimateLabel)} note={monthLabel(month)} />
              <Stat label="Last 30 days" value={observedTotal(provider.last30DaysUsd, provider.last30DaysComplete, estimateLabel)} /></>
            : <><Stat label="Tokens this month" value={tokensLabel(provider.monthToDateTokens)} note={monthLabel(month)} />
              <Stat label="Tokens, last 30 days" value={tokensLabel(provider.last30DaysTokens)} note="No cost recorded" /></>}
          <Stat label="Last active" value={lastActive} />
        </div>
        <div className="mt-6"><p className="obs-chart-summary mb-3">Daily {figure}, last 30 days</p>
          {points.length ? <ObservationChart label={`${name} daily ${figure}`} series={[{ label: costed ? 'Estimated spend' : 'Tokens', points }]} from={points[0].ts - DAY / 2} to={points[points.length - 1].ts + DAY / 2} bars={costed ? { bucketSeconds: DAY, valueHeading: 'Estimated spend (USD)' } : { bucketSeconds: DAY, valueHeading: 'Tokens', format: compactInt }} /> : null}
        </div>
      </div>
      <div className="obs-panel-note"><p>{costed ? caption(harness) : noCostCaption(harness)}</p></div>
    </section>
    <section className="obs-panel mt-5" aria-label={`${name} models this month`}>
      <div className="obs-panel-heading obs-panel-heading-table"><h2>Models this month</h2></div>
      {provider.models.length ? <div className="overflow-x-auto"><table className="obs-data-table"><thead><tr><th>Model</th><th>Requests</th><th>Input</th><th>Cache read</th><th>Output</th><th>Est. cost</th></tr></thead><tbody>
        {provider.models.map((m) => <tr key={m.model}><td>{m.model}</td><td>{fmtInt.format(m.requests)}</td><td>{compactInt(m.input)}</td><td>{compactInt(m.cacheRead)}</td><td>{compactInt(m.output)}</td><td>{observedTotal(m.costUsd, m.costComplete, estimateLabel)}</td></tr>)}
      </tbody></table></div> : <div className="obs-panel-body"><p className="obs-muted text-[13px]">No requests this month.</p></div>}
    </section>
  </>;
}

// PaygAccountSpend is a pay-as-you-go account's overview. The account's
// provider is its harness and its account is the inference provider
// (opencode:baseten), and GetPayAsYouGo groups one harness by the same name,
// so this picks that provider's row.
export function PaygAccountSpend({ account }: { account: AccountSnapshot }) {
  const harness = account.provider;
  const fetchPayg = useCallback(() => obsClient.getPayAsYouGo({ profile: '', harness }), [harness]);
  const { data, error } = usePoll(fetchPayg);
  const provider = Array.isArray(data?.providers) ? data.providers.find((p) => p.provider === account.account) : undefined;
  const name = accountLabel(account);
  return <div>
    <div className="obs-toolbar mt-0"><h2 className="obs-section-title">{costRecorded(account) ? 'Estimated spend' : 'Token usage'}</h2><span className="obs-muted text-xs">{costRecorded(account) ? `No allowance. ${name} bills per token.` : 'No allowance, and no cost recorded.'}</span></div>
    {error ? <Alert tone="error">{data ? 'Refresh failed. Showing the last response. ' : 'Estimated spend could not be loaded. '}{error}</Alert> : null}
    {data?.errors.map((e, i) => <Alert key={i}>{e.section}: {e.error}</Alert>)}
    {!data && !error ? <p className="obs-muted text-[13px]">Loading estimated spend…</p> : null}
    {data && !provider ? <section className="obs-panel"><div className="obs-panel-body"><p className="obs-muted text-[13px]">No usage recorded for this provider.</p></div></section> : null}
    {data && provider ? <Spend provider={provider} month={data.month} name={name} harness={harness} /> : null}
  </div>;
}
