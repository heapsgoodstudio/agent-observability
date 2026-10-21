import { createClient, type Client, type Transport } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-web';
import { ObsService } from '../gen/claudeobs/v1/obs_pb';

export type ObsClient = Client<typeof ObsService>;

// Builds an ObsService client against any transport. Every page imports the browser
// singleton below; tests call this directly with an in-memory transport instead
// (see client.test.ts's createRouterTransport round trip).
export function createObsClient(transport: Transport): ObsClient {
  return createClient(ObsService, transport);
}

let browserClient: ObsClient | undefined;

// The real transport reads window.location.origin, so building it is deferred to
// first use rather than done at module scope: Next's static export prerenders
// every page once in Node (no `window`) before hydrating in the browser, and an
// eager `createConnectTransport` call here would throw during that prerender and
// break `just build`.
function getBrowserClient(): ObsClient {
  if (typeof window === 'undefined') {
    throw new Error('obsClient is browser-only: window is undefined');
  }
  if (!browserClient) {
    browserClient = createObsClient(createConnectTransport({ baseUrl: window.location.origin }));
  }
  return browserClient;
}

// One client every page uses. A Proxy defers construction (and the window
// access above) to the first property access, so importing this module is
// always safe and only calling it requires a browser.
export const obsClient: ObsClient = new Proxy({} as ObsClient, {
  get: (_target, prop, receiver) => Reflect.get(getBrowserClient(), prop, receiver),
});
