'use client';

import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react';
import Link from 'next/link';
import { obsClient } from '@/src/lib/client';
import type { ConfiguredProfile, GetSettingsResponse } from '@/src/gen/claudeobs/v1/obs_pb';
import { Banner } from '@/src/components/ui/Banner';
import { describeErr } from '@/src/lib/presentation';
import { agoText, fmtInt } from '@/src/lib/format';
import { useNow } from '@/src/lib/useNow';
import { usePoll } from '@/src/lib/usePoll';
import { PROFILES_CHANGED_EVENT, detectedHarness, providerLabel, providerMark } from '@/src/lib/providers';
import { useAccounts } from '@/src/components/AccountContext';
import { accountLabel, sidebarFigure } from '@/src/lib/dashboard';

// A detected-provider harness (Oh My Pi, opencode) is named by its harness and
// its accounts are the inference providers detected in its data, so it asks
// for no name and has no wiring to repair
// (internal/profiles.normalizeDetected is the server's half of the rule).
const detectedProviders = detectedHarness;
const DEFAULT_DIR_PLACEHOLDER: Record<string, string> = { claude: '~/.claude', codex: '~/.codex', omp: '~/.omp/agent', opencode: '~/.local/share/opencode' };

const INPUT = 'obs-input w-full disabled:text-muted';
const BUTTON = 'obs-button text-[13px]! disabled:cursor-wait disabled:opacity-50';
const ACTION = 'rounded-sm px-1 py-1 text-[13px] text-secondary hover:bg-hoverbg hover:text-primary disabled:cursor-wait disabled:opacity-40';
type Draft = Pick<ConfiguredProfile, 'id' | 'provider' | 'name' | 'directory' | 'enabled'>;
const emptyDraft = (): Draft => ({ id: '', provider: 'claude', name: '', directory: '', enabled: true });

function profileState(profile: ConfiguredProfile) {
  return !profile.enabled ? 'disabled' : profile.status || 'awaiting data';
}

function ProfileStatus({ profile }: { profile: ConfiguredProfile }) {
  const state = profileState(profile);
  const healthy = ['wired', 'connected', 'ok'].includes(state);
  return <span className={`inline-flex items-center gap-1.5 text-[13px] ${healthy ? 'text-ok-text' : state === 'disabled' ? 'text-muted' : 'text-warn-text'}`}>
    <span aria-hidden="true" className={`h-1.5 w-1.5 shrink-0 rounded-full ${healthy ? 'bg-ok' : state === 'disabled' ? 'bg-muted' : 'bg-warn'}`} />
    {state.replaceAll('_', ' ')}
  </span>;
}

export function Settings() {
  const [settings, setSettings] = useState<GetSettingsResponse | null>(null);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [editing, setEditing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [disconnect, setDisconnect] = useState<string | null>(null);
  const nameInput = useRef<HTMLInputElement>(null);
  const addButton = useRef<HTMLButtonElement>(null);
  const confirmButton = useRef<HTMLButtonElement>(null);
  const returnFocus = useRef<HTMLButtonElement | null>(null);
  const pendingFocus = useRef<HTMLButtonElement | null>(null);
  const mutationInFlight = useRef(false);
  const readSequence = useRef(0);
  const now = useNow();
  // Account observations enrich the registry roster; they never decide which
  // configured profiles remain visible or manageable.
  const fetchProfileMetadata = useCallback(() => obsClient.listProfiles({}), []);
  const metadata = usePoll(fetchProfileMetadata);
  const accounts = useAccounts();
  const detectedAccounts = (harness: string) => (accounts.data?.accounts ?? []).filter((a) => a.provider === harness).toSorted((a, b) => accountLabel(a).localeCompare(accountLabel(b)));

  useEffect(() => {
    if (editing) nameInput.current?.focus();
  }, [editing, draft.id]);

  useEffect(() => {
    if (disconnect) confirmButton.current?.focus();
  }, [disconnect]);

  useEffect(() => {
    if (!busy && pendingFocus.current) {
      const target = pendingFocus.current;
      pendingFocus.current = null;
      if (target.isConnected) target.focus();
      else addButton.current?.focus();
    }
  }, [busy]);

  const refresh = useCallback(async () => {
    const sequence = ++readSequence.current;
    const next = await obsClient.getSettings({});
    if (sequence === readSequence.current) setSettings(next);
    return next;
  }, []);

  // History import is asynchronous. Poll faster while it is running, and
  // refresh the registry when returning to the page or changing a profile.
  useEffect(() => {
    let cancelled = false;
    let inFlight = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      if (inFlight || cancelled || mutationInFlight.current) return;
      clearTimeout(timer);
      inFlight = true;
      let interval = 20000;
      const sequence = readSequence.current + 1;
      try {
        const next = await refresh();
        if (!cancelled && sequence === readSequence.current) setLoadError(null);
        if (next.historyImport?.state === 'running' || next.historyImport?.state === 'queued') interval = 2000;
      } catch (e) {
        if (!cancelled && sequence === readSequence.current) setLoadError(describeErr(e));
      } finally {
        inFlight = false;
        if (!cancelled) timer = setTimeout(poll, interval);
      }
    };
    void poll();
    window.addEventListener('focus', poll);
    window.addEventListener(PROFILES_CHANGED_EVENT, poll);
    return () => {
      cancelled = true;
      ++readSequence.current;
      clearTimeout(timer);
      window.removeEventListener('focus', poll);
      window.removeEventListener(PROFILES_CHANGED_EVENT, poll);
    };
  }, [refresh]);

  async function mutate(action: () => Promise<string>) {
    if (mutationInFlight.current) return;
    mutationInFlight.current = true;
    ++readSequence.current;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const message = await action();
      setNotice(message);
      await refresh();
      setLoadError(null);
    } catch (e) {
      setError(describeErr(e));
    } finally {
      mutationInFlight.current = false;
      setBusy(false);
      window.dispatchEvent(new Event(PROFILES_CHANGED_EVENT));
    }
  }

  function addProfile() {
    returnFocus.current = addButton.current;
    setDraft({ ...emptyDraft(), directory: settings?.claudeDefaultDir ?? '' });
    setEditing(true);
    requestAnimationFrame(() => nameInput.current?.focus());
  }

  function closeEditor() {
    setDraft(emptyDraft());
    setEditing(false);
    if (mutationInFlight.current) {
      pendingFocus.current = returnFocus.current ?? addButton.current;
      return;
    }
    requestAnimationFrame(() => {
      const target = returnFocus.current;
      if (target?.isConnected) target.focus();
      else addButton.current?.focus();
    });
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    const named = !detectedProviders(draft.provider);
    if ((named && !draft.name.trim()) || !draft.directory.trim()) {
      setError(named ? 'Enter a display name and a local directory.' : 'Enter a local directory.');
      return;
    }
    await mutate(async () => {
      const result = await obsClient.saveProfile({ profile: { ...draft, name: draft.name.trim(), directory: draft.directory.trim() } });
      closeEditor();
      return result.profile?.detail || 'Profile saved.';
    });
  }

  const importStatus = settings?.historyImport;
  const importing = importStatus?.state === 'running' || importStatus?.state === 'queued';
  const enabledCount = settings?.profiles.filter((profile) => profile.enabled).length ?? 0;
  const providers = Array.from(new Set(settings?.profiles.map((profile) => profile.provider) ?? [])).sort();
  const observedProfiles = metadata.error ? [] : metadata.data?.profiles ?? [];

  return (
    <>
      <div className="obs-page space-y-6">
        <div className="obs-heading flex-wrap">
          <div>
            <div className="obs-eyebrow">Workspace settings</div>
            <h1>Settings</h1>
            <p className="obs-muted mt-2">Manage the local profiles behind your account connections.</p>
          </div>
          <button ref={addButton} type="button" className={BUTTON} disabled={busy || !settings} onClick={addProfile}><span aria-hidden="true">+</span> Add profile</button>
        </div>
        <div className="space-y-3" hidden={!error && !loadError && !notice}>
          <div role="alert" hidden={!error}>{error ? <Banner tone="error">{error}</Banner> : null}</div>
          <div role="alert" hidden={!loadError}>{loadError ? <Banner tone="error">{settings ? 'Refresh failed; showing the last settings. ' : 'Could not load settings: '}{loadError}</Banner> : null}</div>
          <div role="status" aria-live="polite" hidden={!notice}>{notice ? <Banner tone="info">{notice}</Banner> : null}</div>
        </div>
        {!settings ? <div className="obs-panel p-6" aria-busy={!loadError}>
          <p className="m-0 text-[14px] text-muted">{loadError ? 'Settings could not be loaded. Your configuration has not been changed.' : 'Loading connected profiles…'}</p>
          {loadError ? <button className={`${BUTTON} mt-4`} type="button" onClick={() => { setLoadError(null); void refresh().catch((e) => setLoadError(describeErr(e))); }}>Retry</button> : null}
        </div> : (
          <div className="grid items-start gap-6 xl:grid-cols-[minmax(0,1fr)_340px]">
            <div className="min-w-0 space-y-5">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <h2 className="m-0 text-[15px] font-medium text-primary">Connected profiles <span className="ml-2 rounded-sm border border-line px-1.5 py-0.5 text-[13px] font-normal text-muted">{settings.profiles.length}</span></h2>
                <span className="text-[13px] text-muted">{enabledCount} enabled</span>
              </div>
              {settings.profiles.length === 0 ? <section className="obs-panel p-6">
                <h3 className="m-0 text-[15px] font-medium text-primary">No profiles connected</h3>
                <p className="mb-0 mt-2 text-[14px] leading-6 text-muted">Add a Claude, Codex, Oh My Pi or opencode directory to start monitoring. Existing indexed history remains available in Activity.</p>
                <button className={`${BUTTON} mt-4`} type="button" onClick={addProfile}>Add your first profile</button>
              </section> : null}
              {metadata.error ? <p className="m-0 text-[13px] leading-5 text-muted">Account identity could not be refreshed. All configured profiles remain available below.</p> : null}
              {providers.map((provider) => {
                const members = settings.profiles.filter((profile) => profile.provider === provider);
                const groups = new Map<string, { account: string; profiles: ConfiguredProfile[] }>();
                for (const profile of members) {
                  const observed = observedProfiles.find((row) => row.profile === profile.id && (row.provider || 'claude') === provider);
                  const account = observed?.account ?? '';
                  // A detected harness's accounts span all of its profiles, so its
                  // profiles form one card under one list of detected providers.
                  const key = detectedProviders(provider) ? 'detected' : account ? `account:${account}` : `profile:${profile.id}`;
                  const group = groups.get(key) ?? { account, profiles: [] };
                  group.profiles.push(profile);
                  groups.set(key, group);
                }
                return <section key={provider} aria-label={`${providerLabel(provider)} connections`} className="space-y-3">
                  <div className="flex items-center gap-2 text-secondary"><span aria-hidden="true" className={provider === 'codex' ? 'text-ok-text' : 'text-warn-text'}>{providerMark(provider)}</span><h3 className="m-0 text-[14px] font-medium">{providerLabel(provider)}</h3><span className="ml-auto text-[13px] text-muted">{members.length} {members.length === 1 ? 'profile' : 'profiles'}</span></div>
                  {Array.from(groups.entries()).map(([key, group]) => {
                    const lead = group.profiles[0];
                    return <section key={key} className="obs-panel overflow-hidden" aria-label={`${lead.name || lead.id} account connections`}>
                      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-hairline bg-raised px-5 py-4">
                        {detectedProviders(provider)
                          ? <div className="min-w-0"><h4 className="m-0 text-[14px] font-medium text-primary">Detected providers</h4><p className="mb-0 mt-1 break-all text-[13px] text-muted">{detectedAccounts(provider).length ? 'Switch off a provider to hide it from the sidebar and Overview. Nothing is deleted.' : 'None yet. Providers appear after their first request.'}</p></div>
                          : <div className="min-w-0"><h4 className="m-0 text-[14px] font-medium text-primary">{lead.name || lead.id}</h4><p className="mb-0 mt-1 break-all text-[13px] text-muted">{group.account ? `Account ${group.account}${group.profiles.length > 1 ? ' · Shared allowance' : ''}` : 'Account identity unavailable'}</p></div>}
                        <span className="text-[13px] text-muted">{group.profiles.length} {group.profiles.length === 1 ? 'profile' : 'profiles'}</span>
                      </header>
                      {detectedProviders(provider) && detectedAccounts(provider).length ? <ul className="m-0 list-none space-y-1 border-b border-hairline px-5 py-3" aria-label={`${providerLabel(provider)} detected providers`}>
                        {detectedAccounts(provider).map((a) => <li key={a.key} className="flex items-center justify-between gap-3 py-1.5 text-[13px]">
                          <label className="flex items-center gap-2 text-secondary"><input type="checkbox" checked={!a.hidden} disabled={busy} aria-label={`Show ${accountLabel(a)}`} onChange={(e) => { const show = e.target.checked; void mutate(async () => {
                            await obsClient.setAccountHidden({ accountKey: a.key, hidden: !show });
                            return `${accountLabel(a)} ${show ? 'shown' : 'hidden'}.`;
                          }); }} /><span className="text-primary">{accountLabel(a)}</span></label>
                          <span className="tabular-nums text-muted">{a.hidden ? 'Hidden' : sidebarFigure(a).text}</span>
                        </li>)}
                      </ul> : null}
                      {group.profiles.map((profile) => <article key={profile.id} aria-label={`${profile.name || profile.id} profile`} className={`border-b border-hairline p-5 last:border-0 ${draft.id === profile.id ? 'bg-accent-bg' : ''}`}>
                        <div className="flex flex-wrap items-center justify-between gap-2">
                          <div className="flex min-w-0 flex-wrap items-center gap-2"><h5 className="m-0 break-all text-[14px] font-medium text-primary">{profile.name || profile.id}</h5></div>
                          <ProfileStatus profile={profile} />
                        </div>
                        <p className="mb-0 mt-2 break-all font-mono text-[13px] text-secondary">{profile.directory}</p>
                        <p className="mb-0 mt-2 text-[13px] leading-5 text-muted">{profile.detail || (!profile.enabled ? 'Collection disabled.' : ['wired', 'connected', 'ok'].includes(profile.status) ? 'Local monitoring configured.' : 'Waiting for a connection check.')}{profile.lastSeen > 0 ? ` · Last activity ${agoText(profile.lastSeen, now)}` : ' · No activity observed yet'}</p>
                        <div className="mt-4 flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
                          <label className="flex items-center gap-2 text-[13px] text-secondary"><input type="checkbox" checked={profile.enabled} disabled={busy} aria-label={`Enable ${profile.name || profile.id}`} onChange={(e) => { const enabled = e.target.checked; void mutate(async () => {
                            const result = await obsClient.saveProfile({ profile: { ...profile, enabled } });
                            return result.profile?.detail || `Profile ${enabled ? 'enabled' : 'disabled'}.`;
                          }); }} />Enabled</label>
                          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                            <button type="button" className={ACTION} disabled={busy} aria-label={`Edit ${profile.name || profile.id}`} onClick={(e) => { returnFocus.current = e.currentTarget; setDraft({ ...profile }); setEditing(true); requestAnimationFrame(() => nameInput.current?.focus()); }}>Edit</button>
                            {!detectedProviders(profile.provider) ? <button type="button" className={ACTION} disabled={busy || !profile.enabled} aria-label={`Repair ${profile.name || profile.id}`} onClick={() => void mutate(async () => {
                              const result = await obsClient.repairProfile({ id: profile.id });
                              return result.profile?.detail || 'Profile repair finished.';
                            })}>Repair</button> : null}
                            <button type="button" className={ACTION} disabled={busy || importing || !profile.enabled} aria-label={`Import history for ${profile.name || profile.id}`} onClick={() => void mutate(async () => {
                              const result = await obsClient.importHistory({ profile: profile.id });
                              return result.historyImport?.message || 'History import started.';
                            })}>Import history</button>
                            <button type="button" className={`${ACTION} hover:text-danger-text`} disabled={busy} aria-label={`Disconnect ${profile.name || profile.id}`} onClick={(e) => { returnFocus.current = e.currentTarget; setDisconnect(profile.id); }}>Disconnect</button>
                          </div>
                        </div>
                        {disconnect === profile.id ? <div className="mt-4 rounded-md border border-line bg-raised p-4 text-[13px] text-primary" role="group" aria-labelledby={`disconnect-${profile.id}`}>
                          <p className="m-0 leading-6" id={`disconnect-${profile.id}`}>Disconnect {profile.name || profile.id}? Collection stops; indexed history is preserved.</p>
                          <div className="mt-3 flex flex-wrap gap-2">
                            <button ref={confirmButton} type="button" className={`${BUTTON} text-danger-text`} disabled={busy} onClick={() => void mutate(async () => {
                              await obsClient.removeProfile({ id: profile.id });
                              setDisconnect(null);
                              if (draft.id === profile.id) closeEditor();
                              pendingFocus.current = addButton.current;
                              return 'Profile disconnected. Indexed history preserved.';
                            })}>{busy ? 'Working…' : 'Confirm disconnect'}</button>
                            <button type="button" className={BUTTON} disabled={busy} onClick={() => { setDisconnect(null); returnFocus.current?.focus(); }}>Cancel</button>
                          </div>
                        </div> : null}
                      </article>)}
                    </section>;
                  })}
                </section>;
              })}
              <p className="m-0 text-[13px] leading-6 text-muted">{providers.some((p) => !detectedProviders(p)) ? 'Profiles on the same account share one allowance. ' : ''}Disabling or disconnecting a profile preserves indexed history.</p>
              <Link href="/health" className="inline-flex text-[13px] text-accent-text hover:underline">View collection health <span aria-hidden="true" className="ml-1">→</span></Link>
            </div>
            <aside className="min-w-0 space-y-5" aria-label="Profile management">
              <section className="obs-panel p-5" aria-labelledby="profile-editor-title">
                <h2 id="profile-editor-title" className="m-0 text-[15px] font-medium text-primary">{draft.id ? 'Edit profile' : 'Add a profile'}</h2>
                <p className="mb-5 mt-2 text-[13px] leading-6 text-muted">{draft.id ? 'Update how this local profile is monitored.' : 'Connect a local directory to monitor its activity.'}</p>
                <form onSubmit={save} className="space-y-4" aria-label={draft.id ? 'Edit profile' : 'Add profile'} aria-busy={busy}>
                  <label className="block space-y-1.5 text-[13px] text-secondary"><span>Provider</span><select className={INPUT} value={draft.provider} disabled={busy || !!draft.id} onChange={(e) => {
                    const provider = e.target.value;
                    setDraft({ ...draft, provider, directory: ({ codex: settings.codexDefaultDir, omp: settings.ompDefaultDir, opencode: settings.opencodeDefaultDir } as Record<string, string>)[provider] ?? settings.claudeDefaultDir });
                  }}>{['claude', 'codex', 'omp', 'opencode'].map((p) => <option key={p} value={p}>{providerLabel(p)}</option>)}</select></label>
                  {!detectedProviders(draft.provider) ? <label className="block space-y-1.5 text-[13px] text-secondary"><span>Display name</span><input ref={nameInput} className={INPUT} value={draft.name} required maxLength={100} disabled={busy} placeholder="e.g. Work" autoComplete="off" onChange={(e) => setDraft({ ...draft, name: e.target.value })} /></label> : null}
                  <label className="block space-y-1.5 text-[13px] text-secondary"><span>Local directory</span><input className={`${INPUT} font-mono`} value={draft.directory} required disabled={busy} spellCheck={false} autoComplete="off" placeholder={({ claude: settings.claudeDefaultDir, codex: settings.codexDefaultDir, omp: settings.ompDefaultDir, opencode: settings.opencodeDefaultDir } as Record<string, string>)[draft.provider] || DEFAULT_DIR_PLACEHOLDER[draft.provider] || '~/.claude'} aria-describedby="profile-directory-help" onChange={(e) => setDraft({ ...draft, directory: e.target.value })} /></label>
                  <p id="profile-directory-help" className="m-0 text-[13px] leading-6 text-muted">{draft.provider === 'omp' ? 'Use the Oh My Pi agent directory. It needs no name: its inference providers are detected from its session logs.' : draft.provider === 'opencode' ? 'Use the opencode data directory, the one holding opencode.db. It needs no name: its inference providers are detected from its database.' : "Use the provider's configuration directory. Add each additional directory manually."}</p>
                  <div className="space-y-4 py-1 text-[13px] text-secondary">
                    <label className="flex items-start gap-2"><input className="mt-1" type="checkbox" checked={draft.enabled} disabled={busy} onChange={(e) => setDraft({ ...draft, enabled: e.target.checked })} /><span>Enable collection<span className="mt-1 block text-muted">Monitor new activity for this profile.</span></span></label>
                  </div>
                  <div className="flex flex-wrap gap-2"><button className={`${BUTTON} obs-button-primary`} disabled={busy} type="submit">{busy ? 'Working…' : draft.id ? 'Save profile' : 'Add profile'}</button>{editing || draft.name || draft.directory ? <button className={BUTTON} disabled={busy} type="button" onClick={closeEditor}>Cancel</button> : null}</div>
                </form>
              </section>
              <section className="obs-panel p-5" aria-label="History import">
                <h2 className="m-0 text-[15px] font-medium text-primary">History import</h2>
                <p className="mb-4 mt-2 text-[13px] leading-6 text-muted">Bring existing sessions into your activity history. Imports run in the background.</p>
                <div role="status" aria-live="polite" className="space-y-2 text-[13px] leading-6 text-secondary">
                  <p className="m-0">{importStatus?.state === 'queued' ? 'Import queued…' : importing ? 'Import running…' : importStatus?.state === 'complete' ? 'Import complete' : importStatus?.state === 'failed' ? 'Import failed' : 'No import running'}{importStatus?.profile ? ` · ${settings.profiles.find((profile) => profile.id === importStatus.profile)?.name || importStatus.profile}` : ''}</p>
                  {importStatus?.message ? <p className="m-0 wrap-break-word text-muted">{importStatus.message}</p> : null}
                  {importStatus?.state === 'complete' ? <p className="m-0">{fmtInt.format(importStatus.sessions)} sessions indexed</p> : null}
                </div>
                <button type="button" className={`${BUTTON} mt-4`} disabled={busy || importing || enabledCount === 0} onClick={() => void mutate(async () => {
                  const result = await obsClient.importHistory({});
                  return result.historyImport?.message || 'History import started for enabled profiles.';
                })}>{importing ? 'Import in progress…' : 'Import enabled profiles'}</button>
                {enabledCount === 0 ? <p className="mb-0 mt-3 text-[13px] leading-6 text-muted">Enable a profile to import its history.</p> : null}
              </section>
            </aside>
          </div>
        )}
      </div>
    </>
  );
}
