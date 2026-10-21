// A single scope's lifecycle. Disposing it prevents a late response from
// publishing into a newly selected account, even if the request cannot abort.
// Focus and timer requests arriving mid-flight coalesce into the response
// already coming. A `fresh` request cannot: it says the data changed after
// that request left, so the in-flight response may predate the change, and it
// earns exactly one follow-up fetch once the current one settles.
export function pollCycle<T>(fetcher: () => Promise<T>, publish: (data: T) => void, fail: (error: unknown) => void, settled: () => void, visible: () => boolean) {
  let disposed = false;
  let inFlight = false;
  let followUp = false;
  async function run(fresh = false): Promise<void> {
    if (disposed || !visible()) return;
    if (inFlight) {
      followUp = followUp || fresh;
      return;
    }
    inFlight = true;
    try {
      const data = await fetcher();
      if (!disposed) publish(data);
    } catch (error) {
      if (!disposed) fail(error);
    } finally {
      inFlight = false;
      if (!disposed && followUp) {
        followUp = false;
        await run();
      } else if (!disposed && visible()) settled();
    }
  }
  return {
    run,
    dispose() { disposed = true; },
  };
}
