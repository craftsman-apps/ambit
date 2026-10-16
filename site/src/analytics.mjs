// PostHog page views for the production site only. The same build is deployed to previews and to
// production, so a build-time switch cannot tell them apart; the host name at run time can.
export const PROJECT_KEY = 'phc_pnq7bRQN9PU6rYYBJcaGW65uHWeDhxEmuqSzBvycLyDm';   // public project key, meant for page source
export const PRODUCTION_HOST = 'ambitproject.dev';

export const isProduction = hostname => hostname === PRODUCTION_HOST;

if (typeof window !== 'undefined' && isProduction(window.location.hostname)) {
  import('posthog-js').then(({default: posthog}) => posthog.init(PROJECT_KEY, {
    api_host: 'https://us.i.posthog.com',
    persistence: 'memory',          // no cookie, no local storage: no consent banner needed
    capture_pageview: 'history_change',   // first load and Docusaurus client-side route changes
    autocapture: false,
    disable_session_recording: true,
    ip: false,                      // do not store the visitor IP address
  }));
}
