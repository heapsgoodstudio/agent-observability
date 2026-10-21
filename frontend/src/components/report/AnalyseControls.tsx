'use client';

import { useRef, useState } from 'react';
import { obsClient } from '@/src/lib/client';
import { Alert } from '@/src/components/AccountUI';
import { connectErrorMessage } from '@/src/lib/sessionDetail';

import type { AnalysisTarget } from '@/src/gen/claudeobs/v1/obs_pb';
import { EvaluatorSelect, initialEvaluator, evaluatorAvailable, type Evaluator } from './EvaluatorSelect';

export const CONTROL_CLASS = 'obs-input min-w-[150px]';
export const PRIMARY_BTN_CLASS = 'obs-button obs-button-primary';

// Both session surfaces dispatch through this control. An error retains the
// selection and retry button, and the synchronous guard prevents double starts.
export function AnalyseControls({
  id, targets, label, busy, onStarted,
}: {
  id: string;
  targets: AnalysisTarget[];
  label: string;
  busy: boolean;
  onStarted: () => void;
}) {
  const [value, setValue] = useState<Evaluator | null>(null);
  const selected = value ?? initialEvaluator(targets);
  const available = evaluatorAvailable(targets, selected);
  const [dispatching, setDispatching] = useState(false);
  const [startErr, setStartErr] = useState<string | null>(null);
  const inFlight = useRef(false);

  async function handleClick() {
    if (inFlight.current || busy || !available) return;
    inFlight.current = true;
    setDispatching(true);
    setStartErr(null);
    try {
      await obsClient.startAnalysis({ id, ...selected });
      onStarted();
    } catch (e) {
      setStartErr(connectErrorMessage(e));
    } finally {
      inFlight.current = false;
      setDispatching(false);
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-end gap-3">
        <div className="w-full"><EvaluatorSelect targets={targets} value={selected} onChange={setValue} disabled={busy || dispatching} /></div>
        <button type="button" disabled={busy || dispatching || !available} onClick={handleClick} className={PRIMARY_BTN_CLASS}>
          {dispatching ? 'Starting analysis…' : busy ? 'Runner busy' : label}
        </button>
      </div>
      {busy && <p className="text-xs text-muted">Another analysis is using the shared runner. This page updates automatically.</p>}
      {startErr && <Alert tone="error">{startErr}</Alert>}
    </div>
  );
}
