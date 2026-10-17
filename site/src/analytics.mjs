// PostHog page views for the production site only. The same build is deployed to previews and to
// production, so a build-time switch cannot tell them apart; the host name at run time can.
export const PROJECT_KEY = 'phc_pnq7bRQN9PU6rYYBJcaGW65uHWeDhxEmuqSzBvycLyDm';   // public project key, meant for page source
export const API_HOST = 'https://us.i.posthog.com';   // scripts/vercel-output.mjs must allow this origin in connect-src
export const PRODUCTION_HOST = 'ambitproject.dev';

export const isProduction = hostname => hostname === PRODUCTION_HOST;

if (typeof window !== 'undefined' && isProduction(window.location.hostname)) {
  import('posthog-js').then(({default: posthog}) => posthog.init(PROJECT_KEY, {
    api_host: API_HOST,
    persistence: 'memory',          // no cookie, no local storage: no consent banner needed
    capture_pageview: 'history_change',   // first load and Docusaurus client-side route changes
    autocapture: false,
    disable_session_recording: true,
    disable_external_dependency_loading: true,   // no remote scripts: script-src stays 'self'
    ip: false,                      // do not store the visitor IP address
  }));
}
