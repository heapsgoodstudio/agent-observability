import { describe, expect, it, vi } from 'vitest';
import { pollCycle } from './pollCycle';
function deferred<T>() { let resolve!: (v: T) => void; let reject!: (e: Error) => void; const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
describe('scope polling lifecycle', () => {
  it('ignores an older account completing after the new account', async () => {
    const a = deferred<string>(); const b = deferred<string>(); const published: string[] = [];
    const oldScope = pollCycle(() => a.promise, (r) => published.push(r), vi.fn(), vi.fn(), () => true);
    const newScope = pollCycle(() => b.promise, (r) => published.push(r), vi.fn(), vi.fn(), () => true);
    const oldRequest = oldScope.run(); oldScope.dispose(); const newRequest = newScope.run();
    b.resolve('account-b'); await newRequest; a.resolve('account-a'); await oldRequest;
    expect(published).toEqual(['account-b']);
  });
  it('coalesces focus and timer requests while a response is in flight', async () => {
    const request = deferred<string>(); const fetch = vi.fn(() => request.promise); const settled = vi.fn();
    const cycle = pollCycle(fetch, vi.fn(), vi.fn(), settled, () => true);
    const first = cycle.run(); await cycle.run(); await cycle.run(); expect(fetch).toHaveBeenCalledTimes(1);
    request.resolve('latest'); await first; expect(settled).toHaveBeenCalledTimes(1);
  });
  it('does not fetch or reschedule hidden tabs; disposal suppresses failures too', async () => {
    let visible = false; const request = deferred<string>(); const fetch = vi.fn(() => request.promise); const fail = vi.fn(); const settled = vi.fn();
    const cycle = pollCycle(fetch, vi.fn(), fail, settled, () => visible);
    await cycle.run(); expect(fetch).not.toHaveBeenCalled(); visible = true; const pending = cycle.run(); cycle.dispose(); request.reject(new Error('late failure')); await pending;
    expect(fail).not.toHaveBeenCalled(); expect(settled).not.toHaveBeenCalled();
  });
  it('fetches once more when fresh data is requested while a response is in flight', async () => {
    const first = deferred<string>(); const second = deferred<string>(); const requests = [first, second];
    const fetch = vi.fn(() => requests[fetch.mock.calls.length - 1].promise); const published: string[] = []; const settled = vi.fn();
    const cycle = pollCycle(fetch, (r) => published.push(r), vi.fn(), settled, () => true);
    const running = cycle.run(); await cycle.run(true); await cycle.run(true); await cycle.run();
    expect(fetch).toHaveBeenCalledTimes(1);
    first.resolve('before the change'); await Promise.resolve(); await Promise.resolve();
    expect(fetch).toHaveBeenCalledTimes(2); // one follow-up, however many fresh requests arrived
    second.resolve('after the change'); await running;
    expect(published).toEqual(['before the change', 'after the change']);
    expect(settled).toHaveBeenCalledTimes(1);
  });
});

