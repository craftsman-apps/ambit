import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';
const sidebars: SidebarsConfig = {
  docs: [
    'index',
    {type: 'category', label: 'Getting started', collapsed: false, items: ['quick-start', 'guides/coding-tools', 'concepts', 'install']},
    {type: 'category', label: 'Guides', collapsed: false, items: [
      'guides/models-per-team', 'guides/mcp-apps', 'guides/request-an-app', 'guides/claude-desktop', 'guides/claude-desktop-mcp', 'guides/spending-limits', 'guides/someone-leaves', 'guides/access-review', 'guides/audit-only', 'guides/sandbox-openshell',
    ]},
    {type: 'category', label: 'Hosted agents (experimental)', collapsed: true, items: [
      'guides/agents/grok-bot', 'guides/agents/copilot-studio', 'guides/agents/agentcore', 'guides/agents/cloudflare',
    ]},
    {type: 'category', label: 'Reference', collapsed: false, items: [
      'cli', 'install-by-hand', 'providers', 'api-keys', 'agents', 'front-door', 'service-accounts', 'presets', 'budgets', 'routing', 'savings', 'offboarding', 'alerts', 'compliance', 'performance', 'upgrading',
    ]},
  ],
};
export default sidebars;
